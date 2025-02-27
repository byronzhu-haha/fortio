// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// Do not add any external dependencies we want to keep fortio minimal.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"fortio.org/fortio/bincommon"
	"fortio.org/fortio/dflag/configmap"
	"fortio.org/fortio/fgrpc"
	"fortio.org/fortio/fhttp"
	"fortio.org/fortio/fnet"
	"fortio.org/fortio/log"
	"fortio.org/fortio/periodic"
	"fortio.org/fortio/stats"
	"fortio.org/fortio/tcprunner"
	"fortio.org/fortio/udprunner"
	"fortio.org/fortio/ui"
	"fortio.org/fortio/version"
)

// -- Start of support for multiple proxies (-P) flags on cmd line.
type proxiesFlagList struct{}

func (f *proxiesFlagList) String() string {
	return ""
}

func (f *proxiesFlagList) Set(value string) error {
	proxies = append(proxies, value)
	return nil
}

// -- End of functions for -P support.

// -- Same for -M.
type httpMultiFlagList struct{}

func (f *httpMultiFlagList) String() string {
	return ""
}

func (f *httpMultiFlagList) Set(value string) error {
	httpMulties = append(httpMulties, value)
	return nil
}

// -- End of -M support.

// Usage to a writer.
func usage(w io.Writer, msgs ...interface{}) {
	_, _ = fmt.Fprintf(w, "Φορτίο %s usage:\n\t%s command [flags] target\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n",
		version.Short(),
		os.Args[0],
		"where command is one of: load (load testing), server (starts ui, http-echo,",
		" redirect, proxies, tcp-echo and grpc ping servers), tcp-echo (only the tcp-echo",
		" server), report (report only UI server), redirect (only the redirect server),",
		" proxies (only the -M and -P configured proxies), grpcping (grpc client),",
		" or curl (single URL debug), or nc (single tcp or udp:// connection),",
		" or version (prints the version).",
		"where target is a url (http load tests) or host:port (grpc health test).")
	bincommon.FlagsUsage(w, msgs...)
}

// Prints usage and error messages with StdErr writer.
func usageErr(msgs ...interface{}) {
	usage(os.Stderr, msgs...)
	os.Exit(1)
}

// Attention: every flag that is common to http client goes to bincommon/
// for sharing between fortio and fcurl binaries

const (
	disabled = "disabled"
)

var (
	defaults = &periodic.DefaultRunnerOptions
	// Very small default so people just trying with random URLs don't affect the target.
	qpsFlag         = flag.Float64("qps", defaults.QPS, "Queries Per Seconds or 0 for no wait/max qps")
	numThreadsFlag  = flag.Int("c", defaults.NumThreads, "Number of connections/goroutine/threads")
	durationFlag    = flag.Duration("t", defaults.Duration, "How long to run the test or 0 to run until ^C")
	percentilesFlag = flag.String("p", "50,75,90,99,99.9", "List of pXX to calculate")
	resolutionFlag  = flag.Float64("r", defaults.Resolution, "Resolution of the histogram lowest buckets in seconds")
	offsetFlag      = flag.Duration("offset", defaults.Offset, "Offset of the histogram data")
	goMaxProcsFlag  = flag.Int("gomaxprocs", 0, "Setting for runtime.GOMAXPROCS, <1 doesn't change the default")
	profileFlag     = flag.String("profile", "", "write .cpu and .mem profiles to `file`")
	grpcFlag        = flag.Bool("grpc", false, "Use GRPC (health check by default, add -ping for ping) for load testing")
	echoPortFlag    = flag.String("http-port", "8080",
		"http echo server port. Can be in the form of host:port, ip:port, `port` or /unix/domain/path.")
	tcpPortFlag = flag.String("tcp-port", "8078",
		"tcp echo server port. Can be in the form of host:port, ip:port, `port` or /unix/domain/path or \""+disabled+"\".")
	udpPortFlag = flag.String("udp-port", "8078",
		"udp echo server port. Can be in the form of host:port, ip:port, `port` or \""+disabled+"\".")
	udpAsyncFlag = flag.Bool("udp-async", false, "if true, udp echo server will use separate go routine to reply")
	grpcPortFlag = flag.String("grpc-port", fnet.DefaultGRPCPort,
		"grpc server port. Can be in the form of host:port, ip:port or `port` or /unix/domain/path or \""+disabled+
			"\" to not start the grpc server.")
	echoDbgPathFlag = flag.String("echo-debug-path", "/debug",
		"http echo server `URI` for debug, empty turns off that part (more secure)")
	jsonFlag = flag.String("json", "",
		"Json output to provided file `path` or '-' for stdout (empty = no json output, unless -a is used)")
	uiPathFlag = flag.String("ui-path", "/fortio/", "http server `URI` for UI, empty turns off that part (more secure)")
	curlFlag   = flag.Bool("curl", false, "Just fetch the content once")
	labelsFlag = flag.String("labels", "",
		"Additional config data/labels to add to the resulting JSON, defaults to target URL and hostname")
	// do not remove the flag for backward compatibility.  Was absolute `path` to the dir containing the static files dir
	// which is now embedded in the binary thanks to that support in golang 1.16.
	_            = flag.String("static-dir", "", "Deprecated/unused `path`.")
	dataDirFlag  = flag.String("data-dir", defaultDataDir, "`Directory` where JSON results are stored/read")
	proxiesFlags proxiesFlagList
	proxies      = make([]string, 0)
	// -M flag.
	httpMultiFlags httpMultiFlagList
	httpMulties    = make([]string, 0)

	defaultDataDir = "."

	allowInitialErrorsFlag = flag.Bool("allow-initial-errors", false, "Allow and don't abort on initial warmup errors")
	abortOnFlag            = flag.Int("abort-on", 0,
		"Http `code` that if encountered aborts the run. e.g. 503 or -1 for socket errors.")
	autoSaveFlag = flag.Bool("a", false, "Automatically save JSON result with filename based on labels & timestamp")
	redirectFlag = flag.String("redirect-port", "8081", "Redirect all incoming traffic to https URL"+
		" (need ingress to work properly). Can be in the form of host:port, ip:port, `port` or \""+disabled+"\" to disable the feature.")
	exactlyFlag = flag.Int64("n", 0,
		"Run for exactly this number of calls instead of duration. Default (0) is to use duration (-t). "+
			"Default is 1 when used as grpc ping count.")
	syncFlag         = flag.String("sync", "", "index.tsv or s3/gcs bucket xml `URL` to fetch at startup for server modes.")
	syncIntervalFlag = flag.Duration("sync-interval", 0, "Refresh the url every given interval (default, no refresh)")

	baseURLFlag = flag.String("base-url", "",
		"base `URL` used as prefix for data/index.tsv generation. (when empty, the url from the first request is used)")
	newMaxPayloadSizeKb = flag.Int("maxpayloadsizekb", fnet.MaxPayloadSize/fnet.KILOBYTE,
		"MaxPayloadSize is the maximum size of payload to be generated by the EchoHandler size= argument. In `Kbytes`.")

	// GRPC related flags
	// To get most debugging/tracing:
	// GODEBUG="http2debug=2" GRPC_GO_LOG_VERBOSITY_LEVEL=99 GRPC_GO_LOG_SEVERITY_LEVEL=info fortio grpcping -loglevel debug ...
	doHealthFlag   = flag.Bool("health", false, "grpc ping client mode: use health instead of ping")
	doPingLoadFlag = flag.Bool("ping", false, "grpc load test: use ping instead of health")
	healthSvcFlag  = flag.String("healthservice", "", "which service string to pass to health check")
	pingDelayFlag  = flag.Duration("grpc-ping-delay", 0, "grpc ping delay in response")
	streamsFlag    = flag.Int("s", 1, "Number of streams per grpc connection")

	maxStreamsFlag = flag.Uint("grpc-max-streams", 0,
		"MaxConcurrentStreams for the grpc server. Default (0) is to leave the option unset.")
	jitterFlag = flag.Bool("jitter", false, "set to true to de-synchronize parallel clients' requests")
	// nc mode flag(s).
	ncDontStopOnCloseFlag = flag.Bool("nc-dont-stop-on-eof", false, "in netcat (nc) mode, don't abort as soon as remote side closes")
	// Mirror origin global setting (should be per destination eventually).
	mirrorOriginFlag = flag.Bool("multi-mirror-origin", true, "Mirror the request url to the target for multi proxies (-M)")
	multiSerialFlag  = flag.Bool("multi-serial-mode", false, "Multi server (-M) requests one at a time instead of parallel mode")
	udpTimeoutFlag   = flag.Duration("udp-timeout", udprunner.UDPTimeOutDefaultValue, "Udp timeout")
)

// nolint: funlen // well yes it's fairly big and lotsa ifs.
func main() {
	flag.Var(&proxiesFlags, "P",
		"Tcp proxies to run, e.g -P \"localport1 dest_host1:dest_port1\" -P \"[::1]:0 www.google.com:443\" ...")
	flag.Var(&httpMultiFlags, "M", "Http multi proxy to run, e.g -M \"localport1 baseDestURL1 baseDestURL2\" -M ...")
	bincommon.SharedMain(usage)
	if len(os.Args) < 2 {
		usageErr("Error: need at least 1 command parameter")
	}
	command := os.Args[1]
	os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	flag.Parse()
	if *bincommon.QuietFlag {
		log.SetLogLevelQuiet(log.Error)
	}
	confDir := *bincommon.ConfigDirectoryFlag
	if confDir != "" {
		if _, err := configmap.Setup(flag.CommandLine, confDir); err != nil {
			log.Critf("Unable to watch config/flag changes in %v: %v", confDir, err)
		}
	}
	fnet.ChangeMaxPayloadSize(*newMaxPayloadSizeKb * fnet.KILOBYTE)
	percList, err := stats.ParsePercentiles(*percentilesFlag)
	if err != nil {
		usageErr("Unable to extract percentiles from -p: ", err)
	}
	baseURL := strings.Trim(*baseURLFlag, " \t\n\r/") // remove trailing slash and other whitespace
	sync := strings.TrimSpace(*syncFlag)
	if sync != "" {
		if !ui.Sync(os.Stdout, sync, *dataDirFlag) {
			os.Exit(1)
		}
	}
	isServer := false
	switch command {
	case "curl":
		fortioLoad(true, nil)
	case "nc":
		fortioNC()
	case "load":
		fortioLoad(*curlFlag, percList)
	case "redirect":
		isServer = true
		fhttp.RedirectToHTTPS(*redirectFlag)
	case "report":
		isServer = true
		if *redirectFlag != disabled {
			fhttp.RedirectToHTTPS(*redirectFlag)
		}
		if !ui.Report(baseURL, *echoPortFlag, *dataDirFlag) {
			os.Exit(1) // error already logged
		}
	case "tcp-echo":
		isServer = true
		fnet.TCPEchoServer("tcp-echo", *tcpPortFlag)
		startProxies()
	case "udp-echo":
		isServer = true
		fnet.UDPEchoServer("udp-echo", *udpPortFlag, *udpAsyncFlag)
		startProxies()
	case "proxies":
		if len(flag.Args()) != 0 {
			usageErr("Error: fortio proxies command only takes -P / -M flags")
		}
		isServer = true
		if startProxies() == 0 {
			usageErr("Error: fortio proxies command needs at least one -P / -M flag")
		}
	case "server":
		isServer = true
		if *tcpPortFlag != disabled {
			fnet.TCPEchoServer("tcp-echo", *tcpPortFlag)
		}
		if *udpPortFlag != disabled {
			fnet.UDPEchoServer("udp-echo", *udpPortFlag, *udpAsyncFlag)
		}
		if *grpcPortFlag != disabled {
			fgrpc.PingServer(*grpcPortFlag, *bincommon.CertFlag, *bincommon.KeyFlag, fgrpc.DefaultHealthServiceName, uint32(*maxStreamsFlag))
		}
		if *redirectFlag != disabled {
			fhttp.RedirectToHTTPS(*redirectFlag)
		}
		if !ui.Serve(baseURL, *echoPortFlag, *echoDbgPathFlag, *uiPathFlag, *dataDirFlag, percList) {
			os.Exit(1) // error already logged
		}
		startProxies()
	case "grpcping":
		grpcClient()
	default:
		usageErr("Error: unknown command ", command)
	}
	if isServer {
		if confDir == "" {
			log.Infof("Note: not using dynamic flag watching (use -config to set watch directory)")
		}
		serverLoop(sync)
	}
}

func serverLoop(sync string) {
	// To get a start time log/timestamp in the logs
	log.Infof("All fortio %s servers started!", version.Long())
	d := *syncIntervalFlag
	if sync != "" && d > 0 {
		log.Infof("Will re-sync data dir every %s", d)
		ticker := time.NewTicker(d)
		defer ticker.Stop()
		for range ticker.C {
			ui.Sync(os.Stdout, sync, *dataDirFlag)
		}
	} else {
		select {}
	}
}

func startProxies() int {
	numProxies := 0
	for _, proxy := range proxies {
		s := strings.SplitN(proxy, " ", 2)
		if len(s) != 2 {
			log.Errf("Invalid syntax for proxy \"%s\", should be \"localAddr destHost:destPort\"", proxy)
		}
		fnet.ProxyToDestination(s[0], s[1])
		numProxies++
	}
	for _, hmulti := range httpMulties {
		s := strings.Split(hmulti, " ")
		if len(s) < 2 {
			log.Errf("Invalid syntax for http multi \"%s\", should be \"localAddr destURL1 destURL2...\"", hmulti)
		}
		mcfg := fhttp.MultiServerConfig{Serial: *multiSerialFlag}
		n := len(s) - 1
		mcfg.Targets = make([]fhttp.TargetConf, n)
		for i := 0; i < n; i++ {
			mcfg.Targets[i].Destination = s[i+1]
			mcfg.Targets[i].MirrorOrigin = *mirrorOriginFlag
		}
		fhttp.MultiServer(s[0], &mcfg)
		numProxies++
	}
	return numProxies
}

func fortioNC() {
	l := len(flag.Args())
	if l != 1 && l != 2 {
		usageErr("Error: fortio nc needs a host:port or host port destination")
	}
	d := flag.Args()[0]
	if l == 2 {
		d = d + ":" + flag.Args()[1]
	}
	err := fnet.NetCat(d, os.Stdin, os.Stderr, !*ncDontStopOnCloseFlag /* stop when server closes connection */)
	if err != nil {
		// already logged but exit with error back to shell/caller
		os.Exit(1)
	}
}

// nolint: funlen // maybe refactor/shorten later.
func fortioLoad(justCurl bool, percList []float64) {
	if len(flag.Args()) != 1 {
		usageErr("Error: fortio load/curl needs a url or destination")
	}
	httpOpts := bincommon.SharedHTTPOptions()
	if justCurl {
		bincommon.FetchURL(httpOpts)
		return
	}
	url := httpOpts.URL
	prevGoMaxProcs := runtime.GOMAXPROCS(*goMaxProcsFlag)
	out := os.Stderr
	qps := *qpsFlag // TODO possibly use translated <=0 to "max" from results/options normalization in periodic/
	_, _ = fmt.Fprintf(out, "Fortio %s running at %g queries per second, %d->%d procs",
		version.Short(), qps, prevGoMaxProcs, runtime.GOMAXPROCS(0))
	if *exactlyFlag > 0 {
		_, _ = fmt.Fprintf(out, ", for %d calls: %s\n", *exactlyFlag, url)
	} else {
		if *durationFlag <= 0 {
			// Infinite mode is determined by having a negative duration value
			*durationFlag = -1
			_, _ = fmt.Fprintf(out, ", until interrupted: %s\n", url)
		} else {
			_, _ = fmt.Fprintf(out, ", for %v: %s\n", *durationFlag, url)
		}
	}
	if qps <= 0 {
		qps = -1 // 0==unitialized struct == default duration, -1 (0 for flag) is max
	}
	labels := *labelsFlag
	if labels == "" {
		hname, _ := os.Hostname()
		shortURL := url
		for _, p := range []string{"https://", "http://"} {
			if strings.HasPrefix(url, p) {
				shortURL = url[len(p):]
				break
			}
		}
		labels = shortURL + " , " + strings.SplitN(hname, ".", 2)[0]
		log.LogVf("Generated Labels: %s", labels)
	}
	ro := periodic.RunnerOptions{
		QPS:         qps,
		Duration:    *durationFlag,
		NumThreads:  *numThreadsFlag,
		Percentiles: percList,
		Resolution:  *resolutionFlag,
		Out:         out,
		Labels:      labels,
		Exactly:     *exactlyFlag,
		Jitter:      *jitterFlag,
		RunID:       *bincommon.RunIDFlag,
		Offset:      *offsetFlag,
	}
	var res periodic.HasRunnerResult
	var err error
	if *grpcFlag {
		o := fgrpc.GRPCRunnerOptions{
			RunnerOptions:      ro,
			Destination:        url,
			CACert:             *bincommon.CACertFlag,
			Insecure:           bincommon.TLSInsecure(),
			Service:            *healthSvcFlag,
			Streams:            *streamsFlag,
			AllowInitialErrors: *allowInitialErrorsFlag,
			Payload:            httpOpts.PayloadString(),
			Delay:              *pingDelayFlag,
			UsePing:            *doPingLoadFlag,
			UnixDomainSocket:   httpOpts.UnixDomainSocket,
		}
		res, err = fgrpc.RunGRPCTest(&o)
	} else if strings.HasPrefix(url, tcprunner.TCPURLPrefix) {
		o := tcprunner.RunnerOptions{
			RunnerOptions: ro,
		}
		o.ReqTimeout = httpOpts.HTTPReqTimeOut
		o.Destination = url
		o.Payload = httpOpts.Payload
		res, err = tcprunner.RunTCPTest(&o)
	} else if strings.HasPrefix(url, udprunner.UDPURLPrefix) {
		o := udprunner.RunnerOptions{
			RunnerOptions: ro,
		}
		o.ReqTimeout = *udpTimeoutFlag
		o.Destination = url
		o.Payload = httpOpts.Payload
		res, err = udprunner.RunUDPTest(&o)
	} else {
		o := fhttp.HTTPRunnerOptions{
			HTTPOptions:        *httpOpts,
			RunnerOptions:      ro,
			Profiler:           *profileFlag,
			AllowInitialErrors: *allowInitialErrorsFlag,
			AbortOn:            *abortOnFlag,
		}
		res, err = fhttp.RunHTTPTest(&o)
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "Aborting because of %v\n", err)
		os.Exit(1)
	}
	rr := res.Result()
	warmup := *numThreadsFlag
	if ro.Exactly > 0 {
		warmup = 0
	}
	_, _ = fmt.Fprintf(out, "All done %d calls (plus %d warmup) %.3f ms avg, %.1f qps\n",
		rr.DurationHistogram.Count,
		warmup,
		1000.*rr.DurationHistogram.Avg,
		rr.ActualQPS)
	jsonFileName := *jsonFlag
	if *autoSaveFlag || len(jsonFileName) > 0 { //nolint: nestif // but probably should breakup this function
		var j []byte
		j, err = json.MarshalIndent(res, "", "  ")
		if err != nil {
			log.Fatalf("Unable to json serialize result: %v", err)
		}
		var f *os.File
		if jsonFileName == "-" {
			f = os.Stdout
			jsonFileName = "stdout"
		} else {
			if len(jsonFileName) == 0 {
				jsonFileName = path.Join(*dataDirFlag, rr.ID()+".json")
			}
			f, err = os.Create(jsonFileName)
			if err != nil {
				log.Fatalf("Unable to create %s: %v", jsonFileName, err)
			}
		}
		n, err := f.Write(append(j, '\n'))
		if err != nil {
			log.Fatalf("Unable to write json to %s: %v", jsonFileName, err)
		}
		if f != os.Stdout {
			err := f.Close()
			if err != nil {
				log.Fatalf("Close error for %s: %v", jsonFileName, err)
			}
		}
		_, _ = fmt.Fprintf(out, "Successfully wrote %d bytes of Json data to %s\n", n, jsonFileName)
	}
}

func grpcClient() {
	if len(flag.Args()) != 1 {
		usageErr("Error: fortio grpcping needs host argument in the form of host, host:port or ip:port")
	}
	host := flag.Arg(0)
	count := int(*exactlyFlag)
	if count <= 0 {
		count = 1
	}
	cert := *bincommon.CACertFlag
	var err error
	if *doHealthFlag {
		_, err = fgrpc.GrpcHealthCheck(host, cert, *healthSvcFlag, count, bincommon.TLSInsecure())
	} else {
		httpOpts := bincommon.SharedHTTPOptions()
		_, err = fgrpc.PingClientCall(host, cert, count, httpOpts.PayloadString(), *pingDelayFlag, httpOpts.Insecure)
	}
	if err != nil {
		// already logged
		os.Exit(1)
	}
}
