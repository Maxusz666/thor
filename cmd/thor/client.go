package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/vechain/thor/api"
	"github.com/vechain/thor/block"
	"github.com/vechain/thor/chain"
	"github.com/vechain/thor/co"
	"github.com/vechain/thor/comm"
	"github.com/vechain/thor/consensus"
	"github.com/vechain/thor/genesis"
	"github.com/vechain/thor/logdb"
	"github.com/vechain/thor/lvldb"
	"github.com/vechain/thor/p2psrv"
	"github.com/vechain/thor/packer"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/txpool"
	cli "gopkg.in/urfave/cli.v1"
)

var (
	version   = "1.0"
	gitCommit string
	release   = "dev"
)

// Options for Client.
type Options struct {
	DataPath    string
	Bind        string
	Proposer    thor.Address
	Beneficiary thor.Address
	PrivateKey  *ecdsa.PrivateKey
}

func newApp() *cli.App {
	app := cli.NewApp()
	app.Version = fmt.Sprintf("%s-%s-commit%s", release, version, gitCommit)
	app.Name = "Thor"
	app.Usage = "Core of VeChain"
	app.Copyright = "2018 VeChain Foundation <https://vechain.org/>"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "port",
			Value: ":55555",
			Usage: "p2p listen address",
		},
		cli.StringFlag{
			Name:  "restfulport",
			Value: ":8081",
			Usage: "p2p listen address",
		},
		cli.StringFlag{
			Name:  "nodekey",
			Usage: "private key (for node) file path (defaults to ~/.thor-node.key if omitted)",
		},
		cli.StringFlag{
			Name:  "key",
			Usage: "private key (for pack) as hex (for testing)",
		},
		cli.StringFlag{
			Name:  "datadir",
			Value: "/tmp/thor_datadir_test",
			Usage: "chain data path",
		},
		cli.IntFlag{
			Name:  "verbosity",
			Value: int(log.LvlInfo),
			Usage: "log verbosity (0-9)",
		},
		cli.StringFlag{
			Name:  "vmodule",
			Usage: "log verbosity pattern",
		},
	}
	app.Action = action

	return app
}

func action(ctx *cli.Context) error {
	glogger := log.NewGlogHandler(log.StreamHandler(os.Stderr, log.TerminalFormat(true)))
	glogger.Verbosity(log.Lvl(ctx.Int("verbosity")))
	glogger.Vmodule(ctx.String("vmodule"))
	log.Root().SetHandler(glogger)

	nodeKey, err := loadNodeKey(ctx)
	if err != nil {
		return err
	}

	proposer, privateKey, err := loadAccount(ctx)
	if err != nil {
		return err
	}

	lv, err := lvldb.New(ctx.String("datadir"), lvldb.Options{})
	if err != nil {
		return err
	}
	defer lv.Close()

	ldb, err := logdb.New(ctx.String("datadir") + "/log.db")
	if err != nil {
		return err
	}
	defer ldb.Close()

	stateCreator := state.NewCreator(lv)

	genesisBlock, _, err := genesis.Dev.Build(stateCreator)
	if err != nil {
		return err
	}

	ch := chain.New(lv)
	if err := ch.WriteGenesis(genesisBlock); err != nil {
		return err
	}

	peerCh := make(chan *p2psrv.Peer)
	srv := p2psrv.New(
		&p2psrv.Options{
			PrivateKey:     nodeKey,
			MaxPeers:       25,
			ListenAddr:     ctx.String("port"),
			BootstrapNodes: []*discover.Node{discover.MustParseNode(boot1), discover.MustParseNode(boot2)},
		})
	srv.SubscribePeer(peerCh)
	cm := comm.New(ch)

	srv.Start("thor@111111", cm.Protocols())
	defer srv.Stop()

	cm.Start(peerCh)
	defer cm.Stop()

	lsr, err := net.Listen("tcp", ctx.String("restfulport"))
	if err != nil {
		return err
	}
	defer lsr.Close()

	txpl := txpool.New()
	txIter, err := txpl.NewIterator(ch, stateCreator)
	if err != nil {
		return err
	}

	var goes co.Goes
	c, cancel := context.WithCancel(context.Background())

	es := &events{
		newBlockPacked:  make(chan *block.Block),
		newBlockAck:     make(chan struct{}),
		bestBlockUpdate: make(chan struct{}),
	}

	goes.Go(func() {
		cs := consensus.New(ch, stateCreator)
		blockCh := make(chan *block.Block)
		sub := cm.SubscribeBlock(blockCh)

		for {
			select {
			case <-c.Done():
				sub.Unsubscribe()
				return
			default:
				es.consent(c, blockCh, cm, ch, cs)
			}
		}
	})

	goes.Go(func() {
		pk := packer.New(ch, stateCreator, proposer, proposer)
		ticker := time.NewTicker(2 * time.Second)
		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
				fmt.Println(cm.IsSynced())
				if cm.IsSynced() {
					es.pack(c, ch, pk, txIter, privateKey)
				}
			}
		}
	})

	goes.Go(func() {
		restful := http.Server{Handler: api.NewHTTPHandler(ch, stateCreator, txpl, ldb)}

		go func() {
			<-c.Done()
			restful.Shutdown(context.TODO())
		}()

		if err := restful.Serve(lsr); err != http.ErrServerClosed {
			log.Error(fmt.Sprintf("%v", err))
		}
	})

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)

	select {
	case <-interrupt:
		cancel()
		goes.Wait()
	}

	return nil
}

func loadNodeKey(ctx *cli.Context) (key *ecdsa.PrivateKey, err error) {
	keyFile := ctx.String("nodekey")
	if keyFile == "" {
		// no file specified, use default file path
		home, err := homeDir()
		if err != nil {
			return nil, err
		}
		keyFile = filepath.Join(home, ".thor-node.key")
	} else if !filepath.IsAbs(keyFile) {
		// resolve to absolute path
		keyFile, err = filepath.Abs(keyFile)
		if err != nil {
			return nil, err
		}
	}

	// try to load from file
	if key, err = crypto.LoadECDSA(keyFile); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		return key, nil
	}

	// no such file, generate new key and write in
	key, err = crypto.GenerateKey()
	if err != nil {
		return nil, err
	}

	if err := crypto.SaveECDSA(keyFile, key); err != nil {
		return nil, err
	}
	return key, nil
}

func loadAccount(ctx *cli.Context) (thor.Address, *ecdsa.PrivateKey, error) {
	keyString := ctx.String("key")
	if keyString != "" {
		key, err := crypto.HexToECDSA(keyString)
		if err != nil {
			return thor.Address{}, nil, err
		}
		return thor.Address(crypto.PubkeyToAddress(key.PublicKey)), key, nil
	}

	index := rand.Intn(len(genesis.Dev.Accounts()))
	return genesis.Dev.Accounts()[index].Address, genesis.Dev.Accounts()[index].PrivateKey, nil
}

func homeDir() (string, error) {
	// try to get HOME env
	if home := os.Getenv("HOME"); home != "" {
		return home, nil
	}

	user, err := user.Current()
	if err != nil {
		return "", err
	}
	if user.HomeDir != "" {
		return user.HomeDir, nil
	}

	return os.Getwd()
}

type events struct {
	newBlockPacked  chan *block.Block
	newBlockAck     chan struct{}
	bestBlockUpdate chan struct{}
}

func (es *events) consent(ctx context.Context, blockCh chan *block.Block, cm *comm.Communicator, ch *chain.Chain, cs *consensus.Consensus) {
	select {
	case blk := <-blockCh:
		if _, err := ch.GetBlockHeader(blk.Header().ID()); !ch.IsNotFound(err) {
			return
		}
		signer, _ := blk.Header().Signer()
		if trunk, _, err := cs.Consent(blk, uint64(time.Now().Unix())); err == nil {
			ch.AddBlock(blk, trunk)
			if trunk {
				log.Info(fmt.Sprintf("received new block(#%v trunk)", blk.Header().Number()), "id", blk.Header().ID(), "size", blk.Size(), "proposer", signer)
				cm.BroadcastBlock(blk)
				select {
				case es.bestBlockUpdate <- struct{}{}:
				default:
				}
			} else {
				log.Info(fmt.Sprintf("received new block(#%v branch)", blk.Header().Number()), "id", blk.Header().ID(), "size", blk.Size(), "proposer", signer)
			}
		} else {
			log.Warn(fmt.Sprintf("received new block(#%v bad)", blk.Header().Number()), "id", blk.Header().ID(), "size", blk.Size(), "proposer", signer, "err", err.Error())
		}
	case blk := <-es.newBlockPacked:
		if trunk, err := cs.IsTrunk(blk.Header()); err == nil {
			ch.AddBlock(blk, trunk)
			if trunk {
				cm.BroadcastBlock(blk)
			}
			es.newBlockAck <- struct{}{}
		}
	case <-ctx.Done():
		return
	}
}

func (es *events) pack(
	ctx context.Context,
	ch *chain.Chain,
	pk *packer.Packer,
	txIter *txpool.Iterator,
	privateKey *ecdsa.PrivateKey) {

	bestBlock, err := ch.GetBestBlock()
	if err != nil {
		return
	}

	now := uint64(time.Now().Unix())
	if ts, adopt, commit, err := pk.Prepare(bestBlock.Header(), now); err == nil {
		waitSec := ts - now
		log.Info(fmt.Sprintf("waiting to propose new block(#%v)", bestBlock.Header().Number()+1), "after", fmt.Sprintf("%vs", waitSec))

		waitTime := time.NewTimer(time.Duration(waitSec) * time.Second)
		defer waitTime.Stop()

		select {
		case <-waitTime.C:
			for txIter.HasNext() {
				err := adopt(txIter.Next())
				if packer.IsGasLimitReached(err) {
					break
				}
			}

			if blk, _, err := commit(privateKey); err == nil {
				log.Info(fmt.Sprintf("proposed new block(#%v)", blk.Header().Number()), "id", blk.Header().ID(), "size", blk.Size())
				es.newBlockPacked <- blk
				<-es.newBlockAck
			}
		case <-es.bestBlockUpdate:
			return
		case <-ctx.Done():
			return
		}
	}
}