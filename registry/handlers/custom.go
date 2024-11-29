package handlers

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/funcx27/skopeo/cmd/cmd"
	"github.com/patrickmn/go-cache"
)

var (
	imagePullMinIntervalKey = "IMAGE_REPULL_MIN_INTERVAL"
	imageCopyModekey        = "IMAGE_COPY_MODE"
	imagePullTimeoutKey     = "IMAGE_PULL_TIMEOUT"
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

}
func manifestDispatcherCustom(ctx *Context, r *http.Request) http.Handler {
	switch getEnv(imageCopyModekey) {
	case "sync":
		imageHandler(r.Method, r.Host, r.URL.Path, ctx.Config.HTTP.Addr)
	case "async":
		go imageHandler(r.Method, r.Host, r.URL.Path, ctx.Config.HTTP.Addr)
	}
	return manifestDispatcher(ctx, r)
}

func imageHandler(httpMethod, host, urlPath, serverAddr string) {
	hostWithoutPort := strings.Split(host, ":")[0]
	if httpMethod != "HEAD" || net.ParseIP(hostWithoutPort) != nil {
		return
	}
	imagePath := strings.TrimPrefix(urlPath, "/v2")
	image := host + strings.ReplaceAll(imagePath, "/manifests/", ":")
	v, pulledOrPulling := imageCache.Get(image)
	if !pulledOrPulling {
		imageCache.SetDefault(image, "pulling")
		log.Println("pulling image", image)
		err := cmd.ImageSync("registry://127.0.0.1"+serverAddr, image)
		if err != nil {
			imageCache.Delete(image)
			log.Println(err)
		}
		imageCache.SetDefault(image, "pulled")
		return
	}
	if v.(string) == "pulling" && getEnv(imageCopyModekey) == "sync" {
		ctx, cancel := context.WithTimeout(context.Background(), imagePullTimeout)
		defer cancel()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if v.(string) == "pulled" {
					return
				}
				log.Printf("%s is pulling ...\n", image)
			case <-ctx.Done():
				log.Printf("%s is pulling timeout\n", image)
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
