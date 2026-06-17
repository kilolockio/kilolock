package provider

import (
	plugin "github.com/hashicorp/go-plugin"
)

// handshake is the magic-cookie pair that every Terraform provider
// expects on stdin from its parent process. It is part of the public
// wire protocol and lives in terraform's own source tree at
// internal/plugin/serve.go; copied here verbatim. Its purpose is to
// make accidental launches of provider binaries by non-Terraform
// tooling print a clear "this is not how to run me" error rather
// than hang waiting for stdin input.
//
// ProtocolVersion is the *minimum* protocol the client offers. We
// advertise 5 and 6 in VersionedPlugins; the provider picks the
// highest it supports.
var handshake = plugin.HandshakeConfig{
	ProtocolVersion:  6,
	MagicCookieKey:   "TF_PLUGIN_MAGIC_COOKIE",
	MagicCookieValue: "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2",
}

// pluginName is the well-known name the provider's RPC server
// registers under. Terraform itself uses "provider"; matching it
// keeps us forward-compatible with future providers that look up
// the client's expected name from this string.
const pluginName = "provider"
