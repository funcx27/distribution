package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
)

func ImageSync(registryServer string, src ...string) error {
	return NewOpts().runCustom(src, registryServer)
}
func NewOpts() *copyOptions {
	fs, global := globalFlags()
	_, sharedOpts := sharedImageFlags()
	_, deprecatedTLSVerifyOpt := deprecatedTLSVerifyFlags()
	srcFlag, srcOpts := imageFlags(global, sharedOpts, deprecatedTLSVerifyOpt, "src-", "screds")
	destFlag, destOpts := imageDestFlags(global, sharedOpts, deprecatedTLSVerifyOpt, "dest-", "dcreds")
	_, retryOpts := retryFlags()
	fs.AddFlagSet(&srcFlag)
	fs.AddFlagSet(&destFlag)
	fs.VisitAll(overrideFlagFunc)
	return &copyOptions{global: global,
		deprecatedTLSVerify: deprecatedTLSVerifyOpt,
		srcImage:            srcOpts,
		destImage:           destOpts,
		retryOpts:           retryOpts,
	}
}

func (opts *copyOptions) runCustom(src []string, destRegistry string) error {
	_, err := os.Stat(os.Getenv("XDG_RUNTIME_DIR"))
	if err != nil {
		os.Setenv("XDG_RUNTIME_DIR", "/tmp/")
	}
	if len(src) == 0 {
		return fmt.Errorf("src images is empty")
	}
	if !strings.HasPrefix(destRegistry, "registry://") {
		return fmt.Errorf("skopeo copy dest registry addr error: %s", destRegistry)
	}
	rx, _ := regexp.Compile("^http(|s)://|file://")
	var files []string
	var images []string
	for _, s := range src {
		if rx.MatchString(s) {
			files = append(files, s)
		} else {
			images = append(images, s)
		}
	}
	for _, file := range files {
		if err := opts.runListFile(file, destRegistry); err != nil {
			return err
		}
	}
	return opts.copyImages(images, destRegistry)
}

func (opts *copyOptions) runListFile(imageListFile string, destRegistry string) error {
	var ir io.Reader
	rx, _ := regexp.Compile("^http(|s)://")
	if rx.MatchString(imageListFile) {
		resp, err := http.Get(imageListFile)
		if err != nil {
			log.Panic(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			log.Fatal("http error code:", resp.StatusCode)
		}
		ir = resp.Body
	} else {
		listFilePath := strings.TrimPrefix(imageListFile, "file://")
		var err error
		ir, err = os.Open(listFilePath)
		if err != nil {
			return errors.New("open image list file error: " + listFilePath)
		}
	}
	var images []string
	scanner := bufio.NewScanner(ir)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || len(line) == 0 {
			continue
		}
		images = append(images, line)
	}
	return opts.copyImages(images, destRegistry)

}

func (opts *copyOptions) copyImages(images []string, destRegistry string) error {
	var errImage []string
	for _, src := range images {
		dest := destImage(src, destRegistry)
		log.Infof("copying image docker://%s to %s\n", src, dest)
		err := opts.run([]string{"docker://" + src, dest}, os.Stdout)
		if err != nil {
			// errArray := strings.Split(err.Error(), ":")
			// errImageWithError = append(errImageWithError, src+" error: "+errArray[len(errArray)-1])
			errImage = append(errImage, src)
			log.Errorf("copy image error: %s\n", err)
		}
	}
	if len(errImage) > 0 {
		return fmt.Errorf("error image list: %s", errImage)
	}
	return nil
}
func destImage(src, dest string) (destImg string) {
	srcArray := strings.SplitN(strings.TrimPrefix(src, "docker://"), "/", 2)
	var err error
	if len(srcArray) == 1 {
		destImg, err = url.JoinPath(dest, srcArray...)
		if err != nil {
			log.Fatal("dest image path error", err)
		}
	} else {
		destImg, err = url.JoinPath(dest, srcArray[1:]...)
		if err != nil {
			log.Fatal("dest image path error", err)
		}
	}
	return strings.Replace(destImg, "registry://", "docker://", 1)

}

func overrideFlagFunc(f *pflag.Flag) {
	if strings.Contains(f.Name, "-tls-verify") {
		f.Value.Set("false")
	}
	if strings.Contains(f.Name, "insecure-policy") {
		f.Value.Set("true")
	}
}
