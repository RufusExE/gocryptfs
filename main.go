package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/rfjakob/gocryptfs/cryptfs"
	"github.com/rfjakob/gocryptfs/pathfs_frontend"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
)

const (
	PROGRAM_NAME = "gocryptfs"

	// Exit codes
	ERREXIT_USAGE      = 1
	ERREXIT_MOUNT      = 3
	ERREXIT_CIPHERDIR  = 6
	ERREXIT_INIT       = 7
	ERREXIT_LOADCONF   = 8
	ERREXIT_PASSWORD   = 9
	ERREXIT_MOUNTPOINT = 10
)

// GitVersion will be set by the build script "build.bash"
var GitVersion = "[version not set - please compile using ./build.bash]"

func initDir(args *argContainer) {
	err := checkDirEmpty(args.cipherdir)
	if err != nil {
		fmt.Printf("Invalid CIPHERDIR: %v\n", err)
		os.Exit(ERREXIT_INIT)
	}

	cryptfs.Info.Printf("Choose a password for protecting your files.\n")
	password := readPasswordTwice()
	err = cryptfs.CreateConfFile(args.config, password, args.plaintextnames)
	if err != nil {
		fmt.Println(err)
		os.Exit(ERREXIT_INIT)
	}
	cryptfs.Info.Printf("The filesystem is now ready for mounting.\n")
	os.Exit(0)
}

func usageText() {
	printVersion()
	fmt.Printf("\n")
	fmt.Printf("Usage: %s -init|-passwd [OPTIONS] CIPHERDIR\n", PROGRAM_NAME)
	fmt.Printf("  or   %s [OPTIONS] CIPHERDIR MOUNTPOINT\n", PROGRAM_NAME)
	fmt.Printf("\nOptions:\n")
	flagSet.PrintDefaults()
}

type argContainer struct {
	debug, init, zerokey, fusedebug, openssl, passwd, foreground, version,
	plaintextnames, quiet bool
	masterkey, mountpoint, cipherdir, cpuprofile, config string
	notifypid                                    int
}

var flagSet *flag.FlagSet

// loadConfig - load the config file "filename", prompting the user for the password
func loadConfig(filename string) (masterkey []byte, confFile *cryptfs.ConfFile) {
	// Check if the file exists at all before prompting for a password
	_, err := os.Stat(filename)
	if err != nil {
		fmt.Print(err)
		os.Exit(ERREXIT_LOADCONF)
	}
	fmt.Printf("Password: ")
	pw := readPassword()
	cryptfs.Info.Printf("Decrypting master key... ")
	cryptfs.Warn.Disable() // Silence DecryptBlock() error messages on incorrect password
	masterkey, confFile, err = cryptfs.LoadConfFile(filename, pw)
	cryptfs.Warn.Enable()
	if err != nil {
		fmt.Println(err)
		fmt.Println("Wrong password.")
		os.Exit(ERREXIT_LOADCONF)
	}
	cryptfs.Info.Printf("done.\n")

	return masterkey, confFile
}

// changePassword - change the password of config file "filename"
func changePassword(filename string) {
	masterkey, confFile := loadConfig(filename)
	fmt.Printf("Please enter your new password.\n")
	newPw := readPasswordTwice()
	confFile.EncryptKey(masterkey, newPw)
	err := confFile.WriteFile()
	if err != nil {
		fmt.Println(err)
		os.Exit(ERREXIT_INIT)
	}
	cryptfs.Info.Printf("Password changed.\n")
	os.Exit(0)
}

// printVersion - print a version string like
// "gocryptfs v0.3.1-31-g6736212-dirty; on-disk format 2"
func printVersion() {
	fmt.Printf("%s %s; on-disk format %d\n", PROGRAM_NAME, GitVersion, cryptfs.HEADER_CURRENT_VERSION)
}

func main() {
	runtime.GOMAXPROCS(4)
	var err error

	// Parse command line arguments
	var args argContainer
	flagSet = flag.NewFlagSet(PROGRAM_NAME, flag.ExitOnError)
	flagSet.Usage = usageText
	flagSet.BoolVar(&args.debug, "debug", false, "Enable debug output")
	flagSet.BoolVar(&args.fusedebug, "fusedebug", false, "Enable fuse library debug output")
	flagSet.BoolVar(&args.init, "init", false, "Initialize encrypted directory")
	flagSet.BoolVar(&args.zerokey, "zerokey", false, "Use all-zero dummy master key")
	flagSet.BoolVar(&args.openssl, "openssl", true, "Use OpenSSL instead of built-in Go crypto")
	flagSet.BoolVar(&args.passwd, "passwd", false, "Change password")
	flagSet.BoolVar(&args.foreground, "f", false, "Stay in the foreground")
	flagSet.BoolVar(&args.version, "version", false, "Print version and exit")
	flagSet.BoolVar(&args.plaintextnames, "plaintextnames", false, "Do not encrypt "+
		"file names - can only be used together with -init")
	flagSet.BoolVar(&args.quiet, "q", false, "Quiet - silence informational messages")
	flagSet.StringVar(&args.masterkey, "masterkey", "", "Mount with explicit master key")
	flagSet.StringVar(&args.cpuprofile, "cpuprofile", "", "Write cpu profile to specified file")
	flagSet.StringVar(&args.config, "config", "", "Use specified config file instead of CIPHERDIR/gocryptfs.conf")
	flagSet.IntVar(&args.notifypid, "notifypid", 0, "Send USR1 to the specified process after "+
		"successful mount - used internally for daemonization")
	flagSet.Parse(os.Args[1:])

	if args.debug {
		cryptfs.Debug.Enable()
		cryptfs.Debug.Printf("Debug output enabled\n")
	}
	// By default, let the child handle everything.
	// The parent *could* handle operations that do not require backgrounding by
	// itself, but that would make the code paths more complicated.
	if !args.foreground {
		forkChild() // does not return
	}
	// Getting here means we *are* the child
	// "-v"
	if args.version {
		printVersion()
		os.Exit(0)
	}
	// Every operation below requires CIPHERDIR. Check that we have it.
	if flagSet.NArg() >= 1 {
		args.cipherdir, _ = filepath.Abs(flagSet.Arg(0))
		err := checkDir(args.cipherdir)
		if err != nil {
			fmt.Printf("Invalid CIPHERDIR: %v\n", err)
			os.Exit(ERREXIT_CIPHERDIR)
		}
	} else {
		usageText()
		os.Exit(ERREXIT_USAGE)
	}
	// "-q"
	if args.quiet {
		cryptfs.Info.Disable()
	}
	// "-config"
	if args.config != "" {
		args.config, err = filepath.Abs(args.config)
		if err != nil {
			fmt.Printf("Invalid \"-config\" setting: %v\n", err)
		}
		cryptfs.Info.Printf("Using config file at custom location %s\n", args.config)
	} else {
		args.config = filepath.Join(args.cipherdir, cryptfs.ConfDefaultName)
	}
	// "-cpuprofile"
	if args.cpuprofile != "" {
		f, err := os.Create(args.cpuprofile)
		if err != nil {
			fmt.Println(err)
			os.Exit(ERREXIT_INIT)
		}
		cryptfs.Info.Printf("Writing CPU profile to %s\n", args.cpuprofile)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	// "-openssl"
	if args.openssl == false {
		cryptfs.Info.Printf("Openssl disabled\n")
	}
	// Operation flags: init, passwd or mount
	// "-init"
	if args.init {
		if flagSet.NArg() > 1 {
			fmt.Printf("Usage: %s -init [OPTIONS] CIPHERDIR\n", PROGRAM_NAME)
			os.Exit(ERREXIT_USAGE)
		}
		initDir(&args) // does not return
	}
	// "-passwd"
	if args.passwd {
		if flagSet.NArg() > 1 {
			fmt.Printf("Usage: %s -passwd [OPTIONS] CIPHERDIR\n", PROGRAM_NAME)
			os.Exit(ERREXIT_USAGE)
		}
		changePassword(args.config) // does not return
	}
	// Mount
	// Check mountpoint
	if flagSet.NArg() < 2 {
		usageText()
		os.Exit(ERREXIT_USAGE)
	}
	args.mountpoint, err = filepath.Abs(flagSet.Arg(1))
	if err != nil {
		fmt.Printf("Invalid MOUNTPOINT: %v\n", err)
		os.Exit(ERREXIT_MOUNTPOINT)
	}
	err = checkDirEmpty(args.mountpoint)
	if err != nil {
		fmt.Printf("Invalid MOUNTPOINT: %v\n", err)
		os.Exit(ERREXIT_MOUNTPOINT)
	}
	// Get master key
	var masterkey []byte
	if args.masterkey != "" {
		// "-masterkey"
		cryptfs.Info.Printf("Using explicit master key.\n")
		masterkey = parseMasterKey(args.masterkey)
		cryptfs.Info.Printf("THE MASTER KEY IS VISIBLE VIA \"ps -auxwww\", ONLY USE THIS MODE FOR EMERGENCIES.\n")
	} else if args.zerokey {
		// "-zerokey"
		cryptfs.Info.Printf("Using all-zero dummy master key.\n")
		cryptfs.Info.Printf("ZEROKEY MODE PROVIDES NO SECURITY AT ALL AND SHOULD ONLY BE USED FOR TESTING.\n")
		masterkey = make([]byte, cryptfs.KEY_LEN)
	} else {
		// Load master key from config file
		var confFile *cryptfs.ConfFile
		masterkey, confFile = loadConfig(args.config)
		printMasterKey(masterkey)
		args.plaintextnames = confFile.PlaintextNames()
	}
	// Initialize FUSE server
	srv := pathfsFrontend(masterkey, args.cipherdir, args.mountpoint, args.fusedebug, args.openssl, args.plaintextnames)
	cryptfs.Info.Println("Filesystem ready.")
	// We are ready - send USR1 signal to our parent
	if args.notifypid > 0 {
		sendUsr1(args.notifypid)
	}
	// Wait for SIGINT in the background and unmount ourselves if we get it.
	// This prevents a dangling "Transport endpoint is not connected" mountpoint.
	handleSigint(srv, args.mountpoint)
	// Jump into server loop. Returns when it gets an umount request from the kernel.
	srv.Serve()
	// main exits with code 0
}

// pathfsFrontend - initialize FUSE server based on go-fuse's PathFS
// Calls os.Exit on errors
func pathfsFrontend(key []byte, cipherdir string, mountpoint string,
	debug bool, openssl bool, plaintextNames bool) *fuse.Server {

	finalFs := pathfs_frontend.NewFS(key, cipherdir, openssl, plaintextNames)
	pathFsOpts := &pathfs.PathNodeFsOptions{ClientInodes: true}
	pathFs := pathfs.NewPathNodeFs(finalFs, pathFsOpts)
	fuseOpts := &nodefs.Options{
		// These options are to be compatible with libfuse defaults,
		// making benchmarking easier.
		NegativeTimeout: time.Second,
		AttrTimeout:     time.Second,
		EntryTimeout:    time.Second,
	}
	conn := nodefs.NewFileSystemConnector(pathFs.Root(), fuseOpts)
	var mOpts fuse.MountOptions
	mOpts.AllowOther = false
	// Set values shown in "df -T" and friends
	// First column, "Filesystem"
	mOpts.Options = append(mOpts.Options, "fsname="+cipherdir)
	// Second column, "Type", will be shown as "fuse." + Name
	mOpts.Name = "gocryptfs"

	srv, err := fuse.NewServer(conn.RawFS(), mountpoint, &mOpts)
	if err != nil {
		fmt.Printf("Mount failed: %v", err)
		os.Exit(ERREXIT_MOUNT)
	}
	srv.SetDebug(debug)

	return srv
}

func handleSigint(srv *fuse.Server, mountpoint string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		err := srv.Unmount()
		if err != nil {
			fmt.Print(err)
			cryptfs.Info.Printf("Trying lazy unmount\n")
			cmd := exec.Command("fusermount", "-u", "-z", mountpoint)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()
		}
		os.Exit(1)
	}()
}
