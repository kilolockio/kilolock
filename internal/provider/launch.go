package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
)

// LaunchOptions tunes Launch behaviour. The zero value is the
// production default: log at INFO level to stderr, no timeout
// (callers provide a context with a deadline if they want one),
// no environment additions.
type LaunchOptions struct {
	// Env is appended to the provider process's environment. Useful
	// for credential indirection (AWS_REGION, GOOGLE_PROJECT, ...).
	// If nil, the parent's environment is inherited unmodified.
	Env []string

	// Stderr, if non-nil, receives the provider's log output
	// (provider binaries emit JSON logs on stderr). When nil,
	// provider stderr is captured by go-plugin's logger and emitted
	// at the chosen Logger level.
	Stderr io.Writer

	// Logger overrides the default hclog logger used for
	// go-plugin's own diagnostic output. When nil, a logger writing
	// to os.Stderr at INFO level is created. The logger does not
	// receive provider stderr unless Stderr is also nil.
	Logger hclog.Logger
}

// Launch starts the provider binary at the given path as a child
// process, completes the plugin handshake, and returns a Client
// bound to whichever protocol version (v5 or v6) the provider
// supports. The caller must Close the returned Client to terminate
// the child process; deferring the Close immediately after Launch
// returns is the conventional pattern.
//
// Launch blocks until the handshake completes or the context is
// cancelled. The context is *not* propagated to the child process's
// lifetime; once handshaken, the child runs until Close is called.
// To cancel a long-running RPC, call Client.Stop from another
// goroutine.
//
// The binary path must be absolute (or resolvable relative to the
// current working directory) and executable by the current process.
// Provider discovery — finding the right binary for a required
// provider source/version — is a separate concern (see ADR 0005;
// added in a later commit).
func Launch(ctx context.Context, binaryPath string, opts LaunchOptions) (Client, error) {
	if binaryPath == "" {
		return nil, errors.New("provider.Launch: empty binary path")
	}
	if info, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("provider.Launch: %w", err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("provider.Launch: %s is a directory, not a binary", binaryPath)
	}

	logger := opts.Logger
	if logger == nil {
		logger = hclog.New(&hclog.LoggerOptions{
			Name:   "provider",
			Output: os.Stderr,
			Level:  hclog.Info,
		})
	}

	cmd := exec.Command(binaryPath)
	if opts.Env != nil {
		cmd.Env = append(os.Environ(), opts.Env...)
	}

	clientCfg := &plugin.ClientConfig{
		HandshakeConfig: handshake,
		VersionedPlugins: map[int]plugin.PluginSet{
			5: {pluginName: pluginV5{}},
			6: {pluginName: pluginV6{}},
		},
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Logger:           logger,
		Managed:          false, // we own Close; do not rely on plugin.CleanupClients
		Stderr:           opts.Stderr,
	}

	pluginClient := plugin.NewClient(clientCfg)

	// rpcClient.Dispense() does NOT take a context; if the handshake
	// hangs, the only way to abort is to Kill the child. Wrap the
	// blocking call so a cancelled context kills the child and
	// returns the cancellation error.
	type result struct {
		raw interface{}
		err error
	}
	done := make(chan result, 1)
	go func() {
		rpc, err := pluginClient.Client()
		if err != nil {
			done <- result{nil, err}
			return
		}
		raw, err := rpc.Dispense(pluginName)
		done <- result{raw, err}
	}()

	var raw interface{}
	select {
	case r := <-done:
		if r.err != nil {
			pluginClient.Kill()
			return nil, fmt.Errorf("provider.Launch: %w", r.err)
		}
		raw = r.raw
	case <-ctx.Done():
		pluginClient.Kill()
		return nil, fmt.Errorf("provider.Launch: %w", ctx.Err())
	}

	negotiated := pluginClient.NegotiatedVersion()
	switch impl := raw.(type) {
	case *clientV5:
		impl.pluginClient = pluginClient
		impl.negotiated = negotiated
		return impl, nil
	case *clientV6:
		impl.pluginClient = pluginClient
		impl.negotiated = negotiated
		return impl, nil
	default:
		pluginClient.Kill()
		return nil, fmt.Errorf("provider.Launch: unexpected dispense type %T", raw)
	}
}
