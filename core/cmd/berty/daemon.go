package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	gqlhandler "github.com/99designs/gqlgen/handler"
	"github.com/gorilla/websocket"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	p2pcrypto "github.com/libp2p/go-libp2p-crypto"
	reuse "github.com/libp2p/go-reuseport"
	"github.com/pkg/errors"
	"github.com/rs/cors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"berty.tech/core"
	nodeapi "berty.tech/core/api/node"
	gql "berty.tech/core/api/node/graphql"
	graph "berty.tech/core/api/node/graphql/graph"
	p2papi "berty.tech/core/api/p2p"
	"berty.tech/core/entity"
	"berty.tech/core/network"
	"berty.tech/core/network/mock"
	"berty.tech/core/network/p2p"
	"berty.tech/core/node"
	"berty.tech/core/sql"
	"berty.tech/core/sql/sqlcipher"
)

var defaultBootstrap = []string{
	"/ip4/104.248.78.238/tcp/4004/ipfs/QmPCbsVWDtLTdCtwfp5ftZ96xccUNe4hegKStgbss8YACT",
}

type daemonOptions struct {
	sql sqlOptions

	bind         string
	hideBanner   bool
	dropDatabase bool
	initOnly     bool
	bindgql      string

	// p2p
	identity  string
	bootstrap []string
	noP2P     bool
	bindP2P   []string
	hop       bool // relay hop
	mdns      bool
}

func daemonSetupFlags(flags *pflag.FlagSet, opts *daemonOptions) {
	flags.BoolVar(&opts.dropDatabase, "drop-database", false, "drop database to force a reinitialization")
	flags.BoolVar(&opts.hideBanner, "hide-banner", false, "hide banner")
	flags.BoolVar(&opts.initOnly, "init-only", false, "stop after node initialization (useful for integration tests")
	flags.BoolVar(&opts.noP2P, "no-p2p", false, "Disable p2p Drier")
	flags.BoolVar(&opts.hop, "hop", false, "enable relay hop (should not be enable for client)")
	flags.BoolVar(&opts.mdns, "mdns", true, "enable mdns discovery")
	flags.StringVar(&opts.bind, "grpc-bind", ":1337", "gRPC listening address")
	flags.StringVar(&opts.bindgql, "gql-bind", ":8700", "Bind graphql api")
	flags.StringVarP(&opts.identity, "p2p-identity", "i", "", "set p2p identity")
	flags.StringSliceVar(&opts.bootstrap, "bootstrap", defaultBootstrap, "boostrap peers")
	flags.StringSliceVar(&opts.bindP2P, "bind-p2p", []string{"/ip4/0.0.0.0/tcp/0"}, "p2p listening address")
}

func newDaemonCommand() *cobra.Command {
	opts := &daemonOptions{}
	cmd := &cobra.Command{
		Use: "daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon(opts)
		},
	}

	sqlSetupFlags(cmd.Flags(), &opts.sql)
	daemonSetupFlags(cmd.Flags(), opts)
	return cmd
}

func daemon(opts *daemonOptions) error {
	errChan := make(chan error)

	interceptors := []grpc.ServerOption{
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpc_ctxtags.StreamServerInterceptor(),
			//grpc_opentracing.StreamServerInterceptor(),
			//grpc_prometheus.StreamServerInterceptor,
			grpc_zap.StreamServerInterceptor(zap.L()),
			//grpc_auth.StreamServerInterceptor(myAuthFunction),
			grpc_recovery.StreamServerInterceptor(),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_ctxtags.UnaryServerInterceptor(),
			//grpc_opentracing.UnaryServerInterceptor(),
			//grpc_prometheus.UnaryServerInterceptor,
			grpc_zap.UnaryServerInterceptor(zap.L()),
			//grpc_auth.UnaryServerInterceptor(myAuthFunction),
			grpc_recovery.UnaryServerInterceptor(),
		)),
	}

	// initialize gRPC
	gs := grpc.NewServer(interceptors...)
	reflection.Register(gs)

	addr, err := net.ResolveTCPAddr("tcp", opts.bind)
	if err != nil {
		return err
	}

	if addr.IP == nil {
		addr.IP = net.IP{127, 0, 0, 1}
	}

	fmt.Printf("%s - %s:%d\n", addr.Network(), addr.IP.String(), addr.Port)
	listener, err := reuse.Listen(addr.Network(), fmt.Sprintf("%s:%d", addr.IP.String(), addr.Port))
	if err != nil {
		return err
	}

	// initialize sql
	db, err := sqlcipher.Open(opts.sql.path, []byte(opts.sql.key))
	if err != nil {
		return errors.Wrap(err, "failed to open sqlcipher")
	}

	defer db.Close()
	if db, err = sql.Init(db); err != nil {
		return errors.Wrap(err, "failed to initialize sql")
	}

	if opts.dropDatabase {
		if err = sql.DropDatabase(db); err != nil {
			return errors.Wrap(err, "failed to drop database")
		}
	}

	if err = sql.Migrate(db); err != nil {
		return errors.Wrap(err, "failed to apply sql migrations")
	}

	var driver network.Driver
	if !opts.noP2P {
		var identity p2p.Option
		if opts.identity == "" {
			identity = p2p.WithRandomIdentity()
		} else {
			bytes, err := base64.StdEncoding.DecodeString(opts.identity)
			if err != nil {
				return errors.Wrap(err, "failed to decode identity opt, should be base64 encoded")
			}

			prvKey, err := p2pcrypto.UnmarshalPrivateKey(bytes)
			if err != nil {
				return errors.Wrap(err, "failed to unmarshal private key")
			}

			identity = p2p.WithIdentity(prvKey)
		}

		p2pOpts := []p2p.Option{
			p2p.WithDefaultMuxers(),
			p2p.WithDefaultPeerstore(),
			p2p.WithDefaultSecurity(),
			p2p.WithDefaultTransports(),
			// @TODO: Allow static identity loaded from a file (useful for relay
			// server for creating static endpoint for bootstrap)
			// p2p.WithIdentity(<key>),
			p2p.WithNATPortMap(), // @T\ODO: Is this a pb on mobile?
			p2p.WithListenAddrStrings(opts.bindP2P...),
			p2p.WithBootstrap(opts.bootstrap...),
			identity,
		}

		if opts.mdns {
			p2pOpts = append(p2pOpts, p2p.WithMDNS())
		}

		if opts.hop {
			p2pOpts = append(p2pOpts, p2p.WithRelayHOP())
		} else {
			p2pOpts = append(p2pOpts, p2p.WithRelayClient())
		}

		driver, err = p2p.NewDriver(context.Background(), p2pOpts...)
		if err != nil {
			return err
		}
		defer func() {
			if err := driver.Close(); err != nil {
				logger().Warn("failed to close network driver", zap.Error(err))
			}
		}()
	}

	if driver == nil {
		driver = mock.NewEnqueuer()
	}

	// initialize node
	n, err := node.New(
		node.WithP2PGrpcServer(gs),
		node.WithNodeGrpcServer(gs),
		node.WithSQL(db),
		node.WithDevice(&entity.Device{Name: "bart"}), // FIXME: get device dynamically
		node.WithNetworkDriver(driver),                // FIXME: use a p2p driver instead
	)
	if err != nil {
		return errors.Wrap(err, "failed to initialize node")
	}
	defer func() {
		if err := n.Close(); err != nil {
			logger().Warn("failed to close node", zap.Error(err))
		}
	}()

	if opts.initOnly {
		return nil
	}

	conn, err := grpc.Dial(opts.bind, grpc.WithInsecure())
	if err != nil {
		return errors.Wrap(err, "failed to dial node")
	}

	resolver := gql.New(nodeapi.NewServiceClient(conn))

	mux := http.NewServeMux()
	mux.Handle("/", gqlhandler.Playground("Berty", "/query"))
	mux.Handle("/query", gqlhandler.GraphQL(graph.NewExecutableSchema(resolver), gqlhandler.WebsocketUpgrader(websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool {
			return true
		},
	})))

	handler := cors.New(cors.Options{
		AllowedOrigins: []string{"*"}, // FIXME: use specific URLs?
		AllowedMethods: []string{"POST"},
		//AllowCredentials: true,
		AllowedHeaders: []string{"authorization", "content-type"},
		ExposedHeaders: []string{"Access-Control-Allow-Origin"},
		//Debug:            true,
	}).Handler(mux)

	go func() {
		errChan <- http.ListenAndServe(opts.bindgql, handler)
	}()

	// start grpc server(s)
	go func() {
		errChan <- gs.Serve(listener)
	}()

	logger().Info("grpc server started",
		zap.String("user-id", n.UserID()),
		zap.String("bind", opts.bind),
		zap.Int("p2p-api", int(p2papi.Version)),
		zap.Int("node-api", int(nodeapi.Version)),
		zap.String("version", core.Version),
	)

	// start node
	go func() {
		errChan <- n.Start()
	}()

	// show banner
	if !opts.hideBanner {
		fmt.Println(banner)
	}

	// signal handling
	signalChan := make(chan os.Signal, 1)
	signal.Notify(
		signalChan,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	go func() {
		for {
			s := <-signalChan
			switch s {
			case syscall.SIGHUP: // kill -SIGHUP XXXX
				log.Println("sighup received")
			case syscall.SIGINT: // kill -SIGINT XXXX or Ctrl+c
				log.Println("sigint received")
				errChan <- nil
			case syscall.SIGTERM: // kill -SIGTERM XXXX (force stop)
				log.Println("sigterm received")
				errChan <- nil
			case syscall.SIGQUIT: // kill -SIGQUIT XXXX (stop and core dump)
				log.Println("sigquit received")
				errChan <- nil
			default:
				errChan <- fmt.Errorf("unknown signal received")
			}
		}
	}()

	// exiting on first goroutine triggering an error
	return <-errChan
}
