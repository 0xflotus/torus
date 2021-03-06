package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coreos/pkg/capnslog"
	"github.com/dustin/go-humanize"
	"github.com/ricochet2200/go-disk-usage/du"
	"github.com/spf13/cobra"

	"github.com/alternative-storage/torus"
	"github.com/alternative-storage/torus/blockset"
	"github.com/alternative-storage/torus/distributor"
	"github.com/alternative-storage/torus/internal/flagconfig"
	"github.com/alternative-storage/torus/models"
	"github.com/alternative-storage/torus/ring"
	"github.com/alternative-storage/torus/tracing"

	// Register all the possible drivers.
	_ "github.com/alternative-storage/torus/block"
	_ "github.com/alternative-storage/torus/metadata/etcd"
	_ "github.com/alternative-storage/torus/metadata/temp"
	_ "github.com/alternative-storage/torus/storage"
	_ "net/http/pprof"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	dataDir     string
	blockDevice string
	httpAddress string
	peerAddress string
	sizeStr     string
	debugInit   bool
	autojoin    bool
	logpkg      string
	cfg         torus.Config

	debug      bool
	version    bool
	completion bool
)

var rootCommand = &cobra.Command{
	Use:    "torusd",
	Short:  "Torus distributed storage",
	Long:   `The torus distributed storage server.`,
	PreRun: configureServer,
	Run: func(cmd *cobra.Command, args []string) {
		err := runServer(cmd, args)
		if err != nil {
			die("%v", err)
		}
	},
}

func init() {
	rootCommand.PersistentFlags().StringVarP(&blockDevice, "block-device", "", "", "Path to a torus formatted block device")
	rootCommand.PersistentFlags().StringVarP(&dataDir, "data-dir", "", "torus-data", "Path to the data directory")
	rootCommand.PersistentFlags().BoolVarP(&debug, "debug", "", false, "Turn on debug output")
	rootCommand.PersistentFlags().BoolVarP(&debugInit, "debug-init", "", false, "Run a default init for the MDS if one doesn't exist")
	rootCommand.PersistentFlags().StringVarP(&httpAddress, "http", "", "", "HTTP endpoint for debug and stats")
	rootCommand.PersistentFlags().StringVarP(&peerAddress, "peer-address", "", "", "Address to listen on for intra-cluster data")
	rootCommand.PersistentFlags().StringVarP(&sizeStr, "size", "", "1GiB", "How much disk space to use for this storage node")
	rootCommand.PersistentFlags().StringVarP(&logpkg, "logpkg", "", "", "Specific package logging")
	rootCommand.PersistentFlags().BoolVarP(&autojoin, "auto-join", "", false, "Automatically join the storage pool")
	rootCommand.PersistentFlags().BoolVarP(&version, "version", "", false, "Print version info and exit")
	rootCommand.PersistentFlags().BoolVarP(&completion, "completion", "", false, "Output bash completion code")
	flagconfig.AddConfigFlags(rootCommand.PersistentFlags())
}

func main() {
	hostname, _ := os.Hostname()
	if err := jaeger.Init("torusd:" + hostname); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := rootCommand.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func configureServer(cmd *cobra.Command, args []string) {
	if version {
		fmt.Printf("torusd\nVersion: %s\n", torus.Version)
		os.Exit(0)
	}
	switch {
	case debug:
		capnslog.SetGlobalLogLevel(capnslog.DEBUG)
	default:
		capnslog.SetGlobalLogLevel(capnslog.INFO)
	}
	if logpkg != "" {
		capnslog.SetGlobalLogLevel(capnslog.NOTICE)
		rl := capnslog.MustRepoLogger("github.com/alternative-storage/torus")
		llc, err := rl.ParseLogLevelConfig(logpkg)
		if err != nil {
			die("error parsing logpkg: %s", err)
		}
		rl.SetLogLevel(llc)
	}

	var (
		err  error
		size uint64
	)
	if strings.Contains(sizeStr, "%") {
		percent, err := parsePercentage(sizeStr)
		if err != nil {
			die("error parsing size %s: %s", sizeStr, err)
		}
		directory, _ := filepath.Abs(dataDir)
		size = du.NewDiskUsage(directory).Size() * percent / 100
	} else {
		size, err = humanize.ParseBytes(sizeStr)
		if err != nil {
			die("error parsing size %s: %s", sizeStr, err)
		}
	}

	cfg = flagconfig.BuildConfigFromFlags()
	cfg.DataDir = dataDir
	cfg.BlockDevice = blockDevice
	cfg.StorageSize = size
}

func parsePercentage(percentString string) (uint64, error) {
	sizePercent := strings.Split(percentString, "%")[0]
	sizeNumber, err := strconv.Atoi(sizePercent)
	if err != nil {
		return 0, err
	}
	if sizeNumber < 1 || sizeNumber > 100 {
		return 0, fmt.Errorf("invalid size %d; must be between 1%% and 100%%", sizeNumber)
	}
	return uint64(sizeNumber), nil
}

func runServer(cmd *cobra.Command, args []string) error {
	if completion {
		cmd.Root().GenBashCompletion(os.Stdout)
		os.Exit(0)
	}

	var (
		srv *torus.Server
		err error
	)
	switch {
	case cfg.MetadataAddress == "":
		srv, err = torus.NewServer(cfg, "temp", "mfile")
	case debugInit:
		err = torus.InitMDS("etcd", cfg, torus.GlobalMetadata{
			BlockSize:        512 * 1024,
			DefaultBlockSpec: blockset.MustParseBlockLayerSpec("crc,base"),
		}, ring.Ketama)
		if err != nil {
			if err == torus.ErrExists {
				fmt.Println("debug-init: Already exists")
			} else {
				return fmt.Errorf("couldn't debug-init: %s", err)
			}
		}
		fallthrough
	case blockDevice != "":
		srv, err = torus.NewServer(cfg, "etcd", "block_device")
	default:
		srv, err = torus.NewServer(cfg, "etcd", "mfile")
	}
	if err != nil {
		return fmt.Errorf("couldn't start: %s", err)
	}

	if autojoin {
		err = doAutojoin(srv)
		if err != nil {
			return fmt.Errorf("couldn't auto-join: %s", err)
		}
	}

	mainClose := make(chan bool)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	if peerAddress != "" {
		var u *url.URL

		u, err = addrToUri(peerAddress)
		if err != nil {
			return fmt.Errorf("couldn't parse peer address %s: %s", peerAddress, err)
		}
		err = distributor.ListenReplication(srv, u)
	} else {
		err = distributor.OpenReplication(srv)
	}

	defer srv.Close()
	go func() {
		for _ = range signalChan {
			fmt.Println("\nReceived an interrupt, stopping services...")
			close(mainClose)
			// return here to call defer srv.Close()
			return
		}
	}()

	if err != nil {
		return fmt.Errorf("couldn't use server: %s", err)
	}
	if httpAddress != "" {
		http.Handle("/metrics", prometheus.Handler())
		http.ListenAndServe(httpAddress, nil)
	}
	// Wait
	<-mainClose
	return nil
}

// doAutojoin automatically adds nodes to the storage pool.
func doAutojoin(s *torus.Server) error {
	for {
		ring, err := s.MDS.GetRing()
		if err != nil {
			return fmt.Errorf("couldn't get ring: %v", err)
		}
		var newRing torus.Ring
		if r, ok := ring.(torus.RingAdder); ok {
			newRing, err = r.AddPeers(torus.PeerInfoList{
				&models.PeerInfo{
					UUID:        s.MDS.UUID(),
					TotalBlocks: s.Blocks.NumBlocks(),
				},
			})
		} else {
			return fmt.Errorf("current ring type cannot support auto-adding")
		}
		if err == torus.ErrExists {
			// We're already a member; we're coming back up.
			return nil
		}
		if err != nil {
			return fmt.Errorf("couldn't add peer to ring: %v", err)
		}
		err = s.MDS.SetRing(newRing)
		if err == torus.ErrNonSequentialRing || err == torus.ErrAgain {
			fmt.Fprintf(os.Stderr, "failed to set ring, try again: %v", err)
			continue
		}
		return err
	}
}

func die(why string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, why+"\n", args...)
	os.Exit(1)
}
