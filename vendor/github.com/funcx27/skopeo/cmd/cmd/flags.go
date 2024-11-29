package cmd

import (
	"context"
	"time"

	commonFlag "github.com/containers/common/pkg/flag"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	"github.com/funcx27/skopeo/version"
	"github.com/spf13/pflag"
)

// gitCommit will be the hash that the binary was built from
// and will be populated by the Makefile

var defaultUserAgent = "skopeo/" + version.Version

type globalOptions struct {
	debug              bool                    // Enable debug output
	tlsVerify          commonFlag.OptionalBool // Require HTTPS and verify certificates (for docker: and docker-daemon:)
	policyPath         string                  // Path to a signature verification policy file
	insecurePolicy     bool                    // Use an "allow everything" signature verification policy
	registriesDirPath  string                  // Path to a "registries.d" registry configuration directory
	overrideArch       string                  // Architecture to use for choosing images, instead of the runtime one
	overrideOS         string                  // OS to use for choosing images, instead of the runtime one
	overrideVariant    string                  // Architecture variant to use for choosing images, instead of the runtime one
	commandTimeout     time.Duration           // Timeout for the command execution
	registriesConfPath string                  // Path to the "registries.conf" file
	tmpDir             string                  // Path to use for big temporary files
}

func globalFlags() (pflag.FlagSet, *globalOptions) {
	opts := globalOptions{}
	fs := pflag.FlagSet{}
	fs.BoolVar(&opts.debug, "debug", false, "enable debug output")
	fs.StringVar(&opts.policyPath, "policy", "", "Path to a trust policy file")
	fs.BoolVar(&opts.insecurePolicy, "insecure-policy", true, "run the tool without any policy check")
	fs.StringVar(&opts.registriesDirPath, "registries.d", "", "use registry configuration files in `DIR` (e.g. for container signature storage)")
	fs.StringVar(&opts.overrideArch, "override-arch", "", "use `ARCH` instead of the architecture of the machine for choosing images")
	fs.StringVar(&opts.overrideOS, "override-os", "", "use `OS` instead of the running OS for choosing images")
	fs.StringVar(&opts.overrideVariant, "override-variant", "", "use `VARIANT` instead of the running architecture variant for choosing images")
	fs.DurationVar(&opts.commandTimeout, "command-timeout", 0, "timeout for the command execution")
	fs.StringVar(&opts.registriesConfPath, "registries-conf", "", "path to the registries.conf file")
	fs.StringVar(&opts.tmpDir, "tmpdir", "", "directory used to store temporary files")
	return fs, &opts
}

// getPolicyContext returns a *signature.PolicyContext based on opts.
func (opts *globalOptions) getPolicyContext() (*signature.PolicyContext, error) {
	var policy *signature.Policy // This could be cached across calls in opts.
	var err error
	if opts.insecurePolicy {
		policy = &signature.Policy{Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()}}
	} else if opts.policyPath == "" {
		policy, err = signature.DefaultPolicy(nil)
	} else {
		policy, err = signature.NewPolicyFromFile(opts.policyPath)
	}
	if err != nil {
		return nil, err
	}
	return signature.NewPolicyContext(policy)
}

// commandTimeoutContext returns a context.Context and a cancellation callback based on opts.
// The caller should usually "defer cancel()" immediately after calling this.
func (opts *globalOptions) commandTimeoutContext() (context.Context, context.CancelFunc) {
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if opts.commandTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, opts.commandTimeout)
	}
	return ctx, cancel
}

// newSystemContext returns a *types.SystemContext corresponding to opts.
// It is guaranteed to return a fresh instance, so it is safe to make additional updates to it.
func (opts *globalOptions) newSystemContext() *types.SystemContext {
	ctx := &types.SystemContext{
		RegistriesDirPath:        opts.registriesDirPath,
		ArchitectureChoice:       opts.overrideArch,
		OSChoice:                 opts.overrideOS,
		VariantChoice:            opts.overrideVariant,
		SystemRegistriesConfPath: opts.registriesConfPath,
		BigFilesTemporaryDir:     opts.tmpDir,
		DockerRegistryUserAgent:  defaultUserAgent,
	}
	if opts.tlsVerify.Present() {
		ctx.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!opts.tlsVerify.Value())
	}
	return ctx
}
