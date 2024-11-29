package handlers

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	registrymiddleware "github.com/distribution/distribution/v3/registry/middleware/registry"
	"github.com/distribution/distribution/v3/registry/storage"
	memorycache "github.com/distribution/distribution/v3/registry/storage/cache/memory"
	"github.com/funcx27/skopeo/cmd/cmd"
	"github.com/patrickmn/go-cache"
)

var (
	imagePullMinIntervalKey = "IMAGE_REPULL_MIN_INTERVAL"
	imageCopyModekey        = "IMAGE_COPY_MODE"
	imagePullTimeoutKey     = "IMAGE_PULL_TIMEOUT"
	imagePrePullListKey     = "IMAGE_LIST_FILE"
	registryMirrorKey       = "DOCKERHUB_MIRROR"
	imageCache              *cache.Cache
	imagePullTimeout        time.Duration
)

func init() {
	rePullInterval := envStrToDuration(imagePullMinIntervalKey)
	imagePullTimeout = envStrToDuration(imagePullTimeoutKey)
	imageCache = cache.New(rePullInterval, time.Hour)
	log.Printf("image re-pull min interval: %s\n", rePullInterval)
	log.Printf("image pull timeout: %s\n", imagePullTimeout)
	log.Printf("image copy mode: %s\n", getEnv(imageCopyModekey))
	log.Printf("dockerhub mirror: %s\n", getEnv(registryMirrorKey))

	imageListFile := getEnv(imagePrePullListKey)
	_, err := os.Stat("/var/lib/registry/.done")
	if imageListFile != "" && err != nil {
		go func() {
			log.Printf("copying image from list file: %s\n", imageListFile)
			err := cmd.ImageSync("registry://127.0.0.1"+getEnv("REGISTRY_HTTP_ADDR"), imageListFile)
			if err != nil {
				log.Println(err)
			} else {
				f, _ := os.Create("/var/lib/registry/.done")
				defer f.Close()
				log.Printf("copying image from list file done")
			}
		}()
	}
}

func (app *App) restCache() error {
	cacheProvider := memorycache.NewInMemoryBlobDescriptorCacheProvider(memorycache.DefaultSize)
	options := registrymiddleware.GetRegistryOptions()
	localOptions := append(options, storage.BlobDescriptorCacheProvider(cacheProvider))
	app.registry, _ = storage.NewRegistry(app, app.driver, localOptions...)
	return nil
}

func manifestDispatcherCustom(ctx *Context, r *http.Request) http.Handler {
	switch getEnv(imageCopyModekey) {
	case "sync":
		imageHandler(r.Method, r.Host, r.URL.Path, ctx.Config.HTTP.Addr, r.RemoteAddr)
	case "async":
		go imageHandler(r.Method, r.Host, r.URL.Path, ctx.Config.HTTP.Addr, r.RemoteAddr)
	}
	return manifestDispatcher(ctx, r)
}

func imageHandler(httpMethod, host, urlPath, serverAddr, remoteAddr string) {
	if httpMethod != "HEAD" {
		return
	}
	hostIsIP := net.ParseIP(strings.Split(host, ":")[0]) != nil
	rex, _ := regexp.Compile("^/v2/library/.*")
	if hostIsIP &&
		(getEnv(registryMirrorKey) == "" || !rex.MatchString(urlPath)) {
		return
	}
	imagePath := strings.TrimPrefix(urlPath, "/v2")
	image := host + strings.ReplaceAll(imagePath, "/manifests/", ":")
	imageArrary := strings.Split(image, "/")
	if imageArrary[1] == "library" {
		imageArrary[0] = getEnv(registryMirrorKey)
		image = path.Join(imageArrary...)
	}
	v, pulledOrPulling := imageCache.Get(image)
	if !pulledOrPulling {
		imageCache.SetDefault(image, "pulling")
		beginTime := time.Now()
		log.Println(remoteAddr, "pulling image:", image)
		err := cmd.ImageSync("registry://127.0.0.1"+serverAddr, image)
		if err != nil {
			imageCache.Delete(image)
			log.Println(err)
			return
		}
		imageCache.SetDefault(image, "pulled")
		log.Printf("%s pulled image: %s in %s\n", remoteAddr, image, time.Since(beginTime))
		return
	}
	if v.(string) == "pulling" && getEnv(imageCopyModekey) == "sync" {
		ctx, cancel := context.WithTimeout(context.Background(), imagePullTimeout)
		defer cancel()
		ticker := time.NewTicker(3 * time.Second)
		start := time.Now()
		defer ticker.Stop()
		for {
			select {
			case t := <-ticker.C:
				v, _ = imageCache.Get(image)
				if v.(string) == "pulled" {
					return
				}

				log.Printf("%s pull %s waiting %.2fs...\n", remoteAddr, image, t.Sub(start).Seconds())
			case <-ctx.Done():
				log.Printf("%s pull %s timeout\n", remoteAddr, image)
				return
			}
		}
	}
}

func getEnv(key string) string {
	if os.Getenv(key) == "" {
		switch key {
		case imagePullMinIntervalKey:
			return "10m"
		case imageCopyModekey:
			return "sync"
		case imagePullTimeoutKey:
			return "5m"
		}
	}
	return os.Getenv(key)
}

func envStrToDuration(envKey string) time.Duration {
	duration, err := time.ParseDuration(getEnv(envKey))
	if err != nil {
		log.Println(envKey + "value error")
		panic(err)
	}
	return duration
}
