// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-policy-agent/opa/cmd/internal/env"
	fileurl "github.com/open-policy-agent/opa/internal/file/url"
	"github.com/open-policy-agent/opa/v1/runtime"
	"github.com/open-policy-agent/opa/v1/server"
	"github.com/open-policy-agent/opa/v1/util"
)

const (
	defaultAddr        = ":8181"          // default listening address for server
	defaultLocalAddr   = "localhost:8181" // default listening address for server bound to localhost
	defaultHistoryFile = ".opa_history"   // default filename for shell history
)

type runCmdParams struct {
	rt                   runtime.Params
	tlsCertFile          string
	tlsPrivateKeyFile    string
	tlsCACertFile        string
	tlsCertRefresh       time.Duration
	ignore               []string
	serverMode           bool
	skipVersionCheck     bool // skipVersionCheck is deprecated. Use disableTelemetry instead
	disableTelemetry     bool
	authentication       *util.EnumFlag
	authorization        *util.EnumFlag
	minTLSVersion        *util.EnumFlag
	logLevel             *util.EnumFlag
	logFormat            *util.EnumFlag
	logTimestampFormat   string
	algorithm            string
	scope                string
	pubKey               string
	pubKeyID             string
	skipBundleVerify     bool
	skipKnownSchemaCheck bool
	excludeVerifyFiles   []string
	cipherSuites         []string
}

func newRunParams() runCmdParams {
	return runCmdParams{
		rt:             runtime.NewParams(),
		authentication: util.NewEnumFlag("off", []string{"token", "tls", "off"}),
		authorization:  util.NewEnumFlag("off", []string{"basic", "off"}),
		minTLSVersion:  util.NewEnumFlag("1.2", []string{"1.0", "1.1", "1.2", "1.3"}),
		logLevel:       util.NewEnumFlag("info", []string{"debug", "info", "error"}),
		logFormat:      util.NewEnumFlag("json", []string{"text", "json", "json-pretty"}),
	}
}

func initRun(root *cobra.Command, brand string) {
	executable := root.Name()
	cmdParams := newRunParams()

	runCommand := &cobra.Command{
		Use:   "run",
		Short: `Start ` + brand + ` in interactive or server mode`,
		Long: `Start an instance of ` + brand + `.

To run the interactive shell:

    $ ` + executable + ` run

To run the server:

    $ ` + executable + ` run -s

The 'run' command starts an instance of the ` + brand + ` runtime. The ` + brand + ` runtime can be
started as an interactive shell or a server.

When the runtime is started as a shell, users can define rules and evaluate
expressions interactively. When the runtime is started as a server, ` + brand + ` exposes
an HTTP API for managing policies, reading and writing data, and executing
queries.

The runtime can be initialized with one or more files that contain policies or
data. If the '--bundle' option is specified the paths will be treated as policy
bundles and loaded following standard bundle conventions. The path can be a
compressed archive file or a directory which will be treated as a bundle.
Without the '--bundle' flag ` + brand + ` will recursively load ALL rego, JSON, and YAML
files.

When loading from directories, only files with known extensions are considered.
The current set of file extensions that ` + brand + ` will consider are:

    .json          # JSON data
    .yaml or .yml  # YAML data
    .rego          # Rego file

Non-bundle data file and directory paths can be prefixed with the desired
destination in the data document with the following syntax:

    <dotted-path>:<file-path>

To set a data file as the input document in the interactive shell use the
"repl.input" path prefix with the input file:

    repl.input:<file-path>

Example:

    $ ` + executable + ` run repl.input:input.json

Which will load the "input.json" file at path "data.repl.input".

Use the "help input" command in the interactive shell to see more options.


File paths can be specified as URLs to resolve ambiguity in paths containing colons:

    $ ` + executable + ` run file:///c:/path/to/data.json

URL paths to remote public bundles (http or https) will be parsed as shorthand
configuration equivalent of using repeated --set flags to accomplish the same:

	$ ` + executable + ` run -s https://example.com/bundles/bundle.tar.gz

The above shorthand command is identical to:

    $ ` + executable + ` run -s --set "services.cli1.url=https://example.com" \
                 --set "bundles.cli1.service=cli1" \
                 --set "bundles.cli1.resource=/bundles/bundle.tar.gz" \
                 --set "bundles.cli1.persist=true"

The 'run' command can also verify the signature of a signed bundle.
A signed bundle is a normal ` + brand + ` bundle that includes a file
named ".signatures.json". For more information on signed bundles
see https://www.openpolicyagent.org/docs/latest/management-bundles/#signing.

The key to verify the signature of signed bundle can be provided
using the --verification-key flag. For example, for RSA family of algorithms,
the command expects a PEM file containing the public key.
For HMAC family of algorithms (eg. HS256), the secret can be provided
using the --verification-key flag.

The --verification-key-id flag can be used to optionally specify a name for the
key provided using the --verification-key flag.

The --signing-alg flag can be used to specify the signing algorithm.
The 'run' command uses RS256 (by default) as the signing algorithm.

The --scope flag can be used to specify the scope to use for
bundle signature verification.

Example:

    $ ` + executable + ` run --verification-key secret --signing-alg HS256 --bundle bundle.tar.gz

The 'run' command will read the bundle "bundle.tar.gz", check the
".signatures.json" file and perform verification using the provided key.
An error will be generated if "bundle.tar.gz" does not contain a ".signatures.json" file.
For more information on the bundle verification process see
https://www.openpolicyagent.org/docs/latest/management-bundles/#signature-verification.

The 'run' command can ONLY be used with the --bundle flag to verify signatures
for existing bundle files or directories following the bundle structure.

To skip bundle verification, use the --skip-verify flag.

The --watch flag can be used to monitor policy and data file-system changes. When a change is detected, the updated policy
and data is reloaded into OPA. Watching individual files (rather than directories) is generally not recommended as some
updates might cause them to be dropped by OPA.

OPA will automatically perform type checking based on a schema inferred from known input documents and report any errors
resulting from the schema check. Currently this check is performed on OPA's Authorization Policy Input document and will
be expanded in the future. To disable this, use the --skip-known-schema-check flag.

The --v0-compatible flag can be used to opt-in to OPA features and behaviors that were the default in OPA v0.x.
Behaviors enabled by this flag include:
- setting OPA's listening address to ":8181" by default, corresponding to listening on every network interface.
- expecting v0 Rego syntax in policy modules instead of the default v1 Rego syntax.

The --tls-cipher-suites flag can be used to specify the list of enabled TLS 1.0–1.2 cipher suites. Note that TLS 1.3
cipher suites are not configurable. Following are the supported TLS 1.0 - 1.2 cipher suites (IANA):
TLS_RSA_WITH_RC4_128_SHA, TLS_RSA_WITH_3DES_EDE_CBC_SHA, TLS_RSA_WITH_AES_128_CBC_SHA, TLS_RSA_WITH_AES_256_CBC_SHA,
TLS_RSA_WITH_AES_128_CBC_SHA256, TLS_RSA_WITH_AES_128_GCM_SHA256, TLS_RSA_WITH_AES_256_GCM_SHA384, TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA, TLS_ECDHE_RSA_WITH_RC4_128_SHA, TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA, TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA, TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256, TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256, TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256

See https://godoc.org/crypto/tls#pkg-constants for more information.
`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return env.CmdFlags.CheckEnvironmentVariables(cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true

			ctx := context.Background()
			addrSetByUser := cmd.Flags().Changed("addr")
			rt, err := initRuntime(ctx, cmdParams, args, addrSetByUser)
			if err != nil {
				fmt.Println("error:", err)
				return err
			}
			return startRuntime(ctx, rt, cmdParams.serverMode)
		},
	}

	addConfigFileFlag(runCommand.Flags(), &cmdParams.rt.ConfigFile)
	runCommand.Flags().BoolVarP(&cmdParams.serverMode, "server", "s", false, "start the runtime in server mode")
	runCommand.Flags().IntVar(&cmdParams.rt.ReadyTimeout, "ready-timeout", 0, "wait (in seconds) for configured plugins before starting server (value <= 0 disables ready check)")
	runCommand.Flags().StringVarP(&cmdParams.rt.HistoryPath, "history", "H", historyPath(brand), "set path of history file")
	cmdParams.rt.Addrs = runCommand.Flags().StringSliceP("addr", "a", []string{defaultLocalAddr}, "set listening address of the server (e.g., [ip]:<port> for TCP, unix://<path> for UNIX domain socket)")
	cmdParams.rt.DiagnosticAddrs = runCommand.Flags().StringSlice("diagnostic-addr", []string{}, "set read-only diagnostic listening address of the server for /health and /metric APIs (e.g., [ip]:<port> for TCP, unix://<path> for UNIX domain socket)")
	cmdParams.rt.UnixSocketPerm = runCommand.Flags().String("unix-socket-perm", "755", "specify the permissions for the Unix domain socket if used to listen for incoming connections")
	runCommand.Flags().BoolVar(&cmdParams.rt.H2CEnabled, "h2c", false, "enable H2C for HTTP listeners")
	runCommand.Flags().StringVarP(&cmdParams.rt.OutputFormat, "format", "f", "pretty", "set shell output format, i.e, pretty, json")
	runCommand.Flags().BoolVarP(&cmdParams.rt.Watch, "watch", "w", false, "watch command line files for changes")
	addV0CompatibleFlag(runCommand.Flags(), &cmdParams.rt.V0Compatible, false)
	addV1CompatibleFlag(runCommand.Flags(), &cmdParams.rt.V1Compatible, false)
	addMaxErrorsFlag(runCommand.Flags(), &cmdParams.rt.ErrorLimit)
	runCommand.Flags().BoolVar(&cmdParams.rt.PprofEnabled, "pprof", false, "enables pprof endpoints")
	runCommand.Flags().StringVar(&cmdParams.tlsCertFile, "tls-cert-file", "", "set path of TLS certificate file")
	runCommand.Flags().StringVar(&cmdParams.tlsPrivateKeyFile, "tls-private-key-file", "", "set path of TLS private key file")
	runCommand.Flags().StringVar(&cmdParams.tlsCACertFile, "tls-ca-cert-file", "", "set path of TLS CA cert file")
	runCommand.Flags().DurationVar(&cmdParams.tlsCertRefresh, "tls-cert-refresh-period", 0, "set certificate refresh period")
	runCommand.Flags().Var(cmdParams.authentication, "authentication", "set authentication scheme")
	runCommand.Flags().Var(cmdParams.authorization, "authorization", "set authorization scheme")
	runCommand.Flags().Var(cmdParams.minTLSVersion, "min-tls-version", "set minimum TLS version to be used by "+brand+"'s server")
	runCommand.Flags().VarP(cmdParams.logLevel, "log-level", "l", "set log level")
	runCommand.Flags().Var(cmdParams.logFormat, "log-format", "set log format")
	runCommand.Flags().StringVar(&cmdParams.logTimestampFormat, "log-timestamp-format", "", "set log timestamp format (OPA_LOG_TIMESTAMP_FORMAT environment variable)")
	runCommand.Flags().IntVar(&cmdParams.rt.GracefulShutdownPeriod, "shutdown-grace-period", 10, "set the time (in seconds) that the server will wait to gracefully shut down")
	runCommand.Flags().IntVar(&cmdParams.rt.ShutdownWaitPeriod, "shutdown-wait-period", 0, "set the time (in seconds) that the server will wait before initiating shutdown")
	runCommand.Flags().BoolVar(&cmdParams.skipKnownSchemaCheck, "skip-known-schema-check", false, "disables type checking on known input schemas")
	runCommand.Flags().StringSliceVar(&cmdParams.cipherSuites, "tls-cipher-suites", []string{}, "set list of enabled TLS 1.0–1.2 cipher suites (IANA)")
	addConfigOverrides(runCommand.Flags(), &cmdParams.rt.ConfigOverrides)
	addConfigOverrideFiles(runCommand.Flags(), &cmdParams.rt.ConfigOverrideFiles)
	addBundleModeFlag(runCommand.Flags(), &cmdParams.rt.BundleMode, false)
	addReadAstValuesFromStoreFlag(runCommand.Flags(), &cmdParams.rt.ReadAstValuesFromStore, false)

	runCommand.Flags().BoolVar(&cmdParams.skipVersionCheck, "skip-version-check", false, "disables anonymous version reporting (see: https://www.openpolicyagent.org/docs/latest/privacy)")
	err := runCommand.Flags().MarkDeprecated("skip-version-check", "\"skip-version-check\" is deprecated. Use \"disable-telemetry\" instead")
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}

	runCommand.Flags().BoolVar(&cmdParams.disableTelemetry, "disable-telemetry", false, "disables anonymous information reporting (see: https://www.openpolicyagent.org/docs/latest/privacy)")
	addIgnoreFlag(runCommand.Flags(), &cmdParams.ignore)

	// bundle verification config
	addVerificationKeyFlag(runCommand.Flags(), &cmdParams.pubKey)
	addVerificationKeyIDFlag(runCommand.Flags(), &cmdParams.pubKeyID, defaultPublicKeyID)
	addSigningAlgFlag(runCommand.Flags(), &cmdParams.algorithm, defaultTokenSigningAlg)
	addBundleVerificationScopeFlag(runCommand.Flags(), &cmdParams.scope)
	addBundleVerificationSkipFlag(runCommand.Flags(), &cmdParams.skipBundleVerify, false)
	addBundleVerificationExcludeFilesFlag(runCommand.Flags(), &cmdParams.excludeVerifyFiles)

	usageTemplate := `Usage:
  {{.UseLine}} [files]

Flags:
{{.LocalFlags.FlagUsages | trimRightSpace}}
`

	runCommand.SetUsageTemplate(usageTemplate)

	root.AddCommand(runCommand)
}

func initRuntime(ctx context.Context, params runCmdParams, args []string, addrSetByUser bool) (*runtime.Runtime, error) {
	authenticationSchemes := map[string]server.AuthenticationScheme{
		"token": server.AuthenticationToken,
		"tls":   server.AuthenticationTLS,
		"off":   server.AuthenticationOff,
	}

	authorizationScheme := map[string]server.AuthorizationScheme{
		"basic": server.AuthorizationBasic,
		"off":   server.AuthorizationOff,
	}

	minTLSVersions := map[string]uint16{
		"1.0": tls.VersionTLS10,
		"1.1": tls.VersionTLS11,
		"1.2": tls.VersionTLS12,
		"1.3": tls.VersionTLS13,
	}

	tlsCertFilePath, err := fileurl.Clean(params.tlsCertFile)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate file path: %w", err)
	}
	tlsPrivateKeyFilePath, err := fileurl.Clean(params.tlsPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate private key file path: %w", err)
	}
	tlsCACertFilePath, err := fileurl.Clean(params.tlsCACertFile)
	if err != nil {
		return nil, fmt.Errorf("invalid CA certificate file path: %w", err)
	}

	cert, err := loadCertificate(tlsCertFilePath, tlsPrivateKeyFilePath)
	if err != nil {
		return nil, err
	}

	params.rt.CertificateFile = tlsCertFilePath
	params.rt.CertificateKeyFile = tlsPrivateKeyFilePath
	params.rt.CertificateRefresh = params.tlsCertRefresh
	params.rt.CertPoolFile = tlsCACertFilePath

	if tlsCACertFilePath != "" {
		pool, err := loadCertPool(tlsCACertFilePath)
		if err != nil {
			return nil, err
		}
		params.rt.CertPool = pool
	}

	params.rt.Authentication = authenticationSchemes[params.authentication.String()]
	params.rt.Authorization = authorizationScheme[params.authorization.String()]
	params.rt.MinTLSVersion = minTLSVersions[params.minTLSVersion.String()]
	params.rt.Certificate = cert

	timestampFormat := params.logTimestampFormat
	if timestampFormat == "" {
		timestampFormat = os.Getenv("OPA_LOG_TIMESTAMP_FORMAT")
	}
	params.rt.Logging = runtime.LoggingConfig{
		Level:           params.logLevel.String(),
		Format:          params.logFormat.String(),
		TimestampFormat: timestampFormat,
	}
	params.rt.Paths = args
	params.rt.Filter = ignored(params.ignore).Apply
	params.rt.EnableVersionCheck = !params.disableTelemetry

	// For backwards compatibility, check if `--skip-version-check` flag set.
	if params.skipVersionCheck {
		params.rt.EnableVersionCheck = false
	}

	params.rt.SkipBundleVerification = params.skipBundleVerify

	bvc, err := buildVerificationConfig(params.pubKey, params.pubKeyID, params.algorithm, params.scope, params.excludeVerifyFiles)
	if err != nil {
		return nil, err
	}
	params.rt.BundleVerificationConfig = bvc

	if params.rt.BundleVerificationConfig != nil && !params.rt.BundleMode {
		return nil, errors.New("enable bundle mode (ie. --bundle) to verify bundle files or directories")
	}

	params.rt.SkipKnownSchemaCheck = params.skipKnownSchemaCheck

	if len(params.cipherSuites) > 0 {
		cipherSuites, err := verifyCipherSuites(params.cipherSuites)
		if err != nil {
			return nil, err
		}

		params.rt.CipherSuites = cipherSuites
	}

	rt, err := runtime.NewRuntime(ctx, params.rt)
	if err != nil {
		return nil, err
	}

	rt.SetDistributedTracingLogging()
	rt.Params.AddrSetByUser = addrSetByUser

	if !addrSetByUser && rt.Params.V0Compatible {
		rt.Params.Addrs = &[]string{defaultAddr}
	}

	return rt, nil
}

func startRuntime(ctx context.Context, rt *runtime.Runtime, serverMode bool) error {
	if serverMode {
		return rt.Serve(ctx)
	}
	return rt.StartREPL(ctx)
}

func verifyCipherSuites(cipherSuites []string) (*[]uint16, error) {
	cipherSuitesMap := map[string]*tls.CipherSuite{}

	for _, c := range tls.CipherSuites() {
		cipherSuitesMap[c.Name] = c
	}

	for _, c := range tls.InsecureCipherSuites() {
		cipherSuitesMap[c.Name] = c
	}

	cipherSuitesIDs := []uint16{}
	for _, c := range cipherSuites {
		val, ok := cipherSuitesMap[c]
		if !ok {
			return nil, fmt.Errorf("invalid cipher suite %v", c)
		}

		// verify no TLS 1.3 cipher suites as they are not configurable
		if slices.Contains(val.SupportedVersions, tls.VersionTLS13) {
			return nil, fmt.Errorf("TLS 1.3 cipher suite \"%v\" is not configurable", c)
		}

		cipherSuitesIDs = append(cipherSuitesIDs, val.ID)
	}

	return &cipherSuitesIDs, nil
}

func historyPath(brand string) string {
	b := strings.ToLower(brand)
	historyFile := strings.Replace(defaultHistoryFile, "opa", b, 1)

	home := os.Getenv("HOME")
	if len(home) == 0 {
		return historyFile
	}
	return path.Join(home, historyFile)
}

func loadCertificate(tlsCertFile, tlsPrivateKeyFile string) (*tls.Certificate, error) {
	if tlsCertFile != "" && tlsPrivateKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(tlsCertFile, tlsPrivateKeyFile)
		if err != nil {
			return nil, err
		}
		return &cert, nil
	} else if tlsCertFile != "" || tlsPrivateKeyFile != "" {
		return nil, errors.New("--tls-cert-file and --tls-private-key-file must be specified together")
	}

	return nil, nil
}

func loadCertPool(tlsCACertFile string) (*x509.CertPool, error) {
	caCertPEM, err := os.ReadFile(tlsCACertFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert file: %v", err)
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(caCertPEM); !ok {
		return nil, fmt.Errorf("failed to parse CA cert %q", tlsCACertFile)
	}
	return pool, nil
}
