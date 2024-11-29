package handlers

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/distribution/distribution/v3/configuration"
	registrymiddleware "github.com/distribution/distribution/v3/registry/middleware/registry"
	"github.com/distribution/distribution/v3/registry/storage"
	memorycache "github.com/distribution/distribution/v3/registry/storage/cache/memory"
	"github.com/funcx27/skopeo/cmd/cmd"
	"github.com/patrickmn/go-cache"
	"gopkg.in/yaml.v3"
)

var (
	imagePullMinIntervalKey    = "IMAGE_REPULL_MIN_INTERVAL"
	imageCopyModekey           = "IMAGE_COPY_MODE"
	imagePullTimeoutKey        = "IMAGE_PULL_TIMEOUT"
	imagePrePullListKey        = "IMAGE_LIST_FILE"
	imageCleanJobIntervalKey   = "IMAGE_CLEAN_INTERVAL"
	imageCleanBeforeDaysKey    = "IMAGE_CLEAN_BEFORE_DAYS"
	imageCleanTagRetainNumsKey = "IMAGE_CLEAN_TAG_RETAIN_NUMS"
	imageCleanProjectKey       = "IMAGE_CLEAN_PROJECT"
	registryMirrorKey          = "DOCKERHUB_MIRROR"
	imageCache                 *cache.Cache
	imagePullTimeout           time.Duration
	projects                   []string
	imageCleanBefore           time.Duration
	registryProxyMap           = map[string]string{}
)

func init() {
	rePullInterval := envStrToDuration(imagePullMinIntervalKey)
	imagePullTimeout = envStrToDuration(imagePullTimeoutKey)
	imageCache = cache.New(rePullInterval, time.Hour)
	trimmedProjects := strings.TrimFunc(getEnv(imageCleanProjectKey), unicode.IsSpace)
	projects = strings.Split(trimmedProjects, ",")
	log.Printf("image re-pull min interval: %s\n", rePullInterval)
	log.Printf("image pull timeout: %s\n", imagePullTimeout)
	log.Printf("image copy mode: %s\n", getEnv(imageCopyModekey))
	log.Printf("dockerhub mirror: %s\n", getEnv(registryMirrorKey))
	b, _ := os.ReadFile("/var/lib/registry/proxy-mapping.yaml")
	yaml.Unmarshal(b, &registryProxyMap)
	if len(registryProxyMap) > 0 {
		s := "registry proxy host mapping: "
		for k, v := range registryProxyMap {
			s += fmt.Sprintf("%s -> %s\n", k, v)
		}
		log.Println(s)
	}
	if getEnv(imageCleanTagRetainNumsKey) != "" {
		days, err := strconv.ParseInt(getEnv(imageCleanBeforeDaysKey), 10, 64)
		if err != nil {
			log.Fatal(err)
		}
		imageCleanBefore = 24 * time.Hour * time.Duration(days)
		log.Printf("image clean interval: %s\n", getEnv(imageCleanJobIntervalKey))
		log.Printf("image clean project: %s\n", projects)
		log.Printf("image clean before: %s\n", imageCleanBefore)
		log.Printf("image clean retain tag nums: %s\n", getEnv(imageCleanTagRetainNumsKey))
	}

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

func (app *App) CleanImage() error {
	imageTagRetainStr := getEnv(imageCleanTagRetainNumsKey)
	if imageTagRetainStr == "" {
		return nil
	}
	imageTagRetainNums, err := strconv.Atoi(imageTagRetainStr)
	if err != nil {
		return err
	}
	interval, err := time.ParseDuration(getEnv(imageCleanJobIntervalKey))
	if err != nil {
		log.Fatal("image clean job err:", err)
	}
	cleanJob := func() {
		log.Println("image clean job started")
		var DeletedPath []string
		for _, project := range projects {
			deletes, _ := CleanImages(app.Config, project, imageTagRetainNums, imageCleanBefore, false)
			DeletedPath = append(DeletedPath, deletes...)
		}
		if len(DeletedPath) == 0 {
			log.Println("no images to clean")
			return
		} else {
			log.Println("image tags deleted")
		}
		time.Sleep(time.Second * 5)
		storage.MarkAndSweep(app.Context, app.driver, app.registry, storage.GCOpts{DryRun: false, RemoveUntagged: true})
		cacheProvider := memorycache.NewInMemoryBlobDescriptorCacheProvider(memorycache.DefaultSize)
		options := registrymiddleware.GetRegistryOptions()
		localOptions := append(options, storage.BlobDescriptorCacheProvider(cacheProvider))
		app.registry, _ = storage.NewRegistry(app, app.driver, localOptions...)
		log.Printf("image cleaned nums: %d", len(DeletedPath))
	}
	cleanJob()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		cleanJob()
	}
	return nil
}

func manifestDispatcherCustom(ctx *Context, r *http.Request) http.Handler {
	switch getEnv(imageCopyModekey) {
	case "sync":
		imageHandler(r, ctx.Config.HTTP.Addr)
	case "async":
		go imageHandler(r, ctx.Config.HTTP.Addr)
	}
	return manifestDispatcher(ctx, r)
}

func imageHandler(r *http.Request, serverAddr string) {
	host := proxyHostMapping(r.Host)
	if r.Method != "HEAD" {
		return
	}
	hostIsIP := net.ParseIP(strings.Split(host, ":")[0]) != nil
	rex, _ := regexp.Compile("^/v2/library/.*")
	if hostIsIP &&
		(getEnv(registryMirrorKey) == "" || !rex.MatchString(r.URL.Path)) {
		return
	}
	imagePath := strings.TrimPrefix(r.URL.Path, "/v2")
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
		log.Println(r.RemoteAddr, "pulling image:", image)
		err := cmd.ImageSync("registry://127.0.0.1"+serverAddr, image)
		if err != nil {
			imageCache.Delete(image)
			log.Println(err)
			return
		}
		imageCache.SetDefault(image, "pulled")
		log.Printf("%s pulled image: %s in %s\n", r.RemoteAddr, image, time.Since(beginTime))
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

				log.Printf("%s pull %s waiting %.2fs...\n", r.RemoteAddr, image, t.Sub(start).Seconds())
			case <-ctx.Done():
				log.Printf("%s pull %s timeout\n", r.RemoteAddr, image)
				return
			}
		}
	}
}

func CleanImages(config *configuration.Configuration, project string, retain int, deleteBefore time.Duration, dryRun bool) (preDeletePath []string, err error) {
	rootDir := config.Storage.Parameters()["rootdirectory"].(string)
	reposPath := path.Join(rootDir, "docker/registry/v2/repositories")
	projectPath := path.Join(reposPath, project)
	type tagTime struct {
		tag  string
		time time.Time
	}
	var paths = map[string][]tagTime{}
	// 使用filepath.Walk遍历目录
	err = filepath.Walk(projectPath, func(fpath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == "current" {
			link := path.Join(fpath, "link")
			// f, _ := os.Stat(link)
			var stat syscall.Stat_t
			err := syscall.Stat(link, &stat)
			if err != nil {
				return nil
			}
			accessTime := time.Unix(stat.Atim.Sec, int64(stat.Atim.Nsec))
			tagPath := path.Dir(fpath)
			paths[path.Dir(tagPath)] = append(paths[path.Dir(tagPath)], tagTime{tag: path.Base(tagPath), time: accessTime})
		}
		return nil
	})
	if err != nil {
		return
	}
	for p, v := range paths {
		if len(v) <= retain {
			continue
		}
		sort.Slice(v, func(i, j int) bool {
			return v[i].time.After(v[j].time)
		})
		for _, t := range v[retain:] {
			tagFilePath := path.Join(p, t.tag)
			if t.time.Add(deleteBefore).Before(time.Now()) {
				preDeletePath = append(preDeletePath, tagFilePath)
			}
		}
	}
	for _, tagFilePath := range preDeletePath {
		image := strings.TrimPrefix(tagFilePath, reposPath+"/")
		image = strings.Replace(image, "_manifests/tags/", "", 1)
		log.Printf("deleting tag: %s \n", image)
	}
	if dryRun {
		return
	}
	for _, p := range preDeletePath {
		os.RemoveAll(p)
	}
	return
}

func getEnv(key string) string {
	var v = map[string]string{
		imagePullMinIntervalKey:  "10m",
		imageCopyModekey:         "sync",
		imagePullTimeoutKey:      "5m",
		imageCleanJobIntervalKey: "1m",
		imageCleanBeforeDaysKey:  "1",
	}
	if os.Getenv(key) == "" {
		return v[key]
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

func proxyHostMapping(host string) string {
	if registryProxyMap[host] != "" {
		return registryProxyMap[host]
	}
	return host
}
