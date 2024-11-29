package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	commonFlag "github.com/containers/common/pkg/flag"
	"github.com/containers/common/pkg/retry"
	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/signature/signer"
	"github.com/containers/image/v5/transports/alltransports"
	encconfig "github.com/containers/ocicrypt/config"
	enchelpers "github.com/containers/ocicrypt/helpers"
)

type copyOptions struct {
	global              *globalOptions
	deprecatedTLSVerify *deprecatedTLSVerifyOption
	srcImage            *imageOptions
	destImage           *imageDestOptions
	retryOpts           *retry.Options
	additionalTags      []string // For docker-archive: destinations, in addition to the name:tag specified as destination, also add these
	removeSignatures    bool     // Do not copy signatures from the source image
	signByFingerprint   string   // Sign the image using a GPG key with the specified fingerprint
	// signBySigstoreParamFile  string                    // Sign the image using a sigstore signature per configuration in a param file
	signBySigstorePrivateKey string // Sign the image using a sigstore private key
	// signPassphraseFile       string                    // Path pointing to a passphrase file when signing (for either signature format, but only one of them)
	// signIdentity             string                    // Identity of the signed image, must be a fully specified docker reference
	digestFile          string                    // Write digest to this file
	format              commonFlag.OptionalString // Force conversion of the image to a specified format
	quiet               bool                      // Suppress output information when copying images
	all                 bool                      // Copy all of the images if the source is a list
	multiArch           commonFlag.OptionalString // How to handle multi architecture images
	preserveDigests     bool                      // Preserve digests during copy
	encryptLayer        []int                     // The list of layers to encrypt
	encryptionKeys      []string                  // Keys needed to encrypt the image
	decryptionKeys      []string                  // Keys needed to decrypt the image
	imageParallelCopies uint                      // Maximum number of parallel requests when copying images
}

// parseMultiArch parses the list processing selection
// It returns the copy.ImageListSelection to use with image.Copy option
func parseMultiArch(multiArch string) (copy.ImageListSelection, error) {
	switch multiArch {
	case "system":
		return copy.CopySystemImage, nil
	case "all":
		return copy.CopyAllImages, nil
	// There is no CopyNoImages value in copy.ImageListSelection, but because we
	// don't provide an option to select a set of images to copy, we can use
	// CopySpecificImages.
	case "index-only":
		return copy.CopySpecificImages, nil
	// We don't expose CopySpecificImages other than index-only above, because
	// we currently don't provide an option to choose the images to copy. That
	// could be added in the future.
	default:
		return copy.CopySystemImage, fmt.Errorf("unknown multi-arch option %q. Choose one of the supported options: 'system', 'all', or 'index-only'", multiArch)
	}
}

func (opts *copyOptions) run(args []string, stdout io.Writer) (retErr error) {
	if len(args) != 2 {
		return errors.New("exactly two arguments expected")
	}
	// opts.deprecatedTLSVerify.warnIfUsed([]string{"--src-tls-verify", "--dest-tls-verify"})
	imageNames := args

	if err := reexecIfNecessaryForImages(imageNames...); err != nil {
		return err
	}

	policyContext, err := opts.global.getPolicyContext()
	if err != nil {
		return fmt.Errorf("error loading trust policy: %v", err)
	}
	defer func() {
		if err := policyContext.Destroy(); err != nil {
			retErr = noteCloseFailure(retErr, "tearing down policy context", err)
		}
	}()

	srcRef, err := alltransports.ParseImageName(imageNames[0])
	if err != nil {
		return fmt.Errorf("invalid source name %s: %v", imageNames[0], err)
	}
	destRef, err := alltransports.ParseImageName(imageNames[1])
	if err != nil {
		return fmt.Errorf("invalid destination name %s: %v", imageNames[1], err)
	}

	sourceCtx, err := opts.srcImage.newSystemContext()
	if err != nil {
		return err
	}
	destinationCtx, err := opts.destImage.newSystemContext()
	if err != nil {
		return err
	}

	var manifestType string
	if opts.format.Present() {
		manifestType, err = parseManifestFormat(opts.format.Value())
		if err != nil {
			return err
		}
	}

	for _, image := range opts.additionalTags {
		ref, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			return fmt.Errorf("error parsing additional-tag '%s': %v", image, err)
		}
		namedTagged, isNamedTagged := ref.(reference.NamedTagged)
		if !isNamedTagged {
			return fmt.Errorf("additional-tag '%s' must be a tagged reference", image)
		}
		destinationCtx.DockerArchiveAdditionalTags = append(destinationCtx.DockerArchiveAdditionalTags, namedTagged)
	}

	ctx, cancel := opts.global.commandTimeoutContext()
	defer cancel()

	if opts.quiet {
		stdout = nil
	}

	imageListSelection := copy.CopySystemImage
	// if opts.multiArch.Present() && opts.all {
	// 	return fmt.Errorf("cannot use --all and --multi-arch flags together")
	// }
	if opts.multiArch.Present() {
		imageListSelection, err = parseMultiArch(opts.multiArch.Value())
		if err != nil {
			return err
		}
	}
	if opts.all {
		imageListSelection = copy.CopyAllImages
	}

	// if len(opts.encryptionKeys) > 0 && len(opts.decryptionKeys) > 0 {
	// 	return fmt.Errorf("--encryption-key and --decryption-key cannot be specified together")
	// }

	var encLayers *[]int
	var encConfig *encconfig.EncryptConfig
	var decConfig *encconfig.DecryptConfig

	// if len(opts.encryptLayer) > 0 && len(opts.encryptionKeys) == 0 {
	// 	return fmt.Errorf("--encrypt-layer can only be used with --encryption-key")
	// }

	if len(opts.encryptionKeys) > 0 {
		// encryption
		p := opts.encryptLayer
		encLayers = &p
		encryptionKeys := opts.encryptionKeys
		ecc, err := enchelpers.CreateCryptoConfig(encryptionKeys, []string{})
		if err != nil {
			return fmt.Errorf("invalid encryption keys: %v", err)
		}
		cc := encconfig.CombineCryptoConfigs([]encconfig.CryptoConfig{ecc})
		encConfig = cc.EncryptConfig
	}

	if len(opts.decryptionKeys) > 0 {
		// decryption
		decryptionKeys := opts.decryptionKeys
		dcc, err := enchelpers.CreateCryptoConfig([]string{}, decryptionKeys)
		if err != nil {
			return fmt.Errorf("invalid decryption keys: %v", err)
		}
		cc := encconfig.CombineCryptoConfigs([]encconfig.CryptoConfig{dcc})
		decConfig = cc.DecryptConfig
	}

	// c/image/copy.Image does allow creating both simple signing and sigstore signatures simultaneously,
	// with independent passphrases, but that would make the CLI probably too confusing.
	// For now, use the passphrase with either, but only one of them.
	// if opts.signPassphraseFile != "" && opts.signByFingerprint != "" && opts.signBySigstorePrivateKey != "" {
	// 	return fmt.Errorf("only one of --sign-by and sign-by-sigstore-private-key can be used with sign-passphrase-file")
	// }
	var passphrase string
	var signers []*signer.Signer
	// if opts.signBySigstoreParamFile != "" {
	// 	signer, err := sigstore.NewSignerFromParameterFile(opts.signBySigstoreParamFile, &sigstore.Options{
	// 		PrivateKeyPassphrasePrompt: func(keyFile string) (string, error) {
	// 			return promptForPassphrase(keyFile, os.Stdin, os.Stdout)
	// 		},
	// 		Stdin:  os.Stdin,
	// 		Stdout: stdout,
	// 	})
	// 	if err != nil {
	// 		return fmt.Errorf("error using --sign-by-sigstore: %w", err)
	// 	}
	// 	defer signer.Close()
	// 	signers = append(signers, signer)
	// }

	// var signIdentity reference.Named = nil
	// if opts.signIdentity != "" {
	// 	signIdentity, err = reference.ParseNamed(opts.signIdentity)
	// 	if err != nil {
	// 		return fmt.Errorf("could not parse --sign-identity: %v", err)
	// 	}
	// }

	opts.destImage.warnAboutIneffectiveOptions(destRef.Transport())

	return retry.IfNecessary(ctx, func() error {
		manifestBytes, err := copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
			RemoveSignatures:                 opts.removeSignatures,
			Signers:                          signers,
			SignBy:                           opts.signByFingerprint,
			SignPassphrase:                   passphrase,
			SignBySigstorePrivateKeyFile:     opts.signBySigstorePrivateKey,
			SignSigstorePrivateKeyPassphrase: []byte(passphrase),
			// SignIdentity:                     signIdentity,
			ReportWriter:          stdout,
			SourceCtx:             sourceCtx,
			DestinationCtx:        destinationCtx,
			ForceManifestMIMEType: manifestType,
			ImageListSelection:    imageListSelection,
			PreserveDigests:       opts.preserveDigests,
			OciDecryptConfig:      decConfig,
			OciEncryptLayers:      encLayers,
			OciEncryptConfig:      encConfig,
			MaxParallelDownloads:  opts.imageParallelCopies,
		})
		if err != nil {
			return err
		}
		if opts.digestFile != "" {
			manifestDigest, err := manifest.Digest(manifestBytes)
			if err != nil {
				return err
			}
			if err = os.WriteFile(opts.digestFile, []byte(manifestDigest.String()), 0644); err != nil {
				return fmt.Errorf("failed to write digest to file %q: %w", opts.digestFile, err)
			}
		}
		return nil
	}, opts.retryOpts)
}
