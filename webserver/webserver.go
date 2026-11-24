package webserver

import (
	"embed"
	"encoding/json"
	"fmt"
	"github.com/aetaric/checkrr/logging"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aetaric/checkrr/check"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
	"github.com/knadh/koanf/v2"
	"github.com/robfig/cron/v3"
	bolt "go.etcd.io/bbolt"
)

//go:embed build
var staticFS embed.FS

var fileInfo [][]string

var baseurl BaseURL

var db *bolt.DB
var scheduler *cron.Cron
var cronEntry cron.EntryID
var checkrrInstance *check.Checkrr
var checkrrLogger *logging.Log
var localizer *i18n.Localizer

type Webserver struct {
	Port           int
	BaseURL        BaseURL
	tls            bool
	cert           string
	key            string
	data           chan []string
	trustedProxies []string
	DB             *bolt.DB
	config         *koanf.Koanf
	FullConfig     *koanf.Koanf
}

type BaseURL string

// EnforceTrailingSlash ensures that the base URL has a trailing slash.
func (b BaseURL) EnforceTrailingSlash() BaseURL {
	if !strings.HasSuffix(string(b), "/") {
		return BaseURL(string(b) + "/")
	}
	return b
}

// String returns the base URL as a string.
func (b BaseURL) String() string {
	return string(b)
}

func (w *Webserver) FromConfig(conf *koanf.Koanf, c chan []string, checkrr *check.Checkrr, l *i18n.Localizer) {
	w.config = conf
	w.Port = conf.Int("port")
	w.tls = conf.Bool("tls")
	if w.tls {
		w.key = conf.String("certs.key")
		w.cert = conf.String("certs.cert")
	}
	w.BaseURL = BaseURL(conf.String("baseurl")).EnforceTrailingSlash()
	baseurl = w.BaseURL
	if conf.Strings("trustedproxies") != nil {
		w.trustedProxies = conf.Strings("trustedproxies")
	} else {
		w.trustedProxies = nil
	}
	w.data = c
	db = w.DB
	checkrrInstance = checkrr
	checkrrLogger = checkrr.Logger

	localizer = l
}

func (w *Webserver) AddScheduler(cron *cron.Cron, entryid cron.EntryID) {
	scheduler = cron
	cronEntry = entryid
}

func (w *Webserver) Run() {
	// Build a waitgroup so we can have a webserver and a chan processor
	wg := new(sync.WaitGroup)
	wg.Add(2)
	// Webserver
	go func() {
		createServer(w)
		wg.Done()
	}()
	// chan processor
	go func() {
		for data := range w.data {
			fileInfo = append(fileInfo, data)
		}
		wg.Done()
	}()

	wg.Wait()
}

func createServer(w *Webserver) *gin.Engine {
	router := setupRouter(w)

	if w.tls {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "WebHTTPSStart",
			TemplateData: map[string]interface{}{
				"Port": w.Port,
			},
		})
		checkrrLogger.Infof(message)
		err := router.RunTLS(fmt.Sprintf(":%v", w.Port), w.cert, w.key)
		if err != nil {
			message := localizer.MustLocalize(&i18n.LocalizeConfig{
				MessageID: "WebTLSFail",
			})
			checkrrLogger.Warn(message)
		}
	} else {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "WebHTTPStart",
			TemplateData: map[string]interface{}{
				"Port": w.Port,
			},
		})
		checkrrLogger.Info(message)
		err := router.Run(fmt.Sprintf(":%v", w.Port))
		if err != nil {
			message := localizer.MustLocalize(&i18n.LocalizeConfig{
				MessageID: "WebFail",
			})
			checkrrLogger.Warn(message)
		}
	}
	return router
}

// setupRouter builds the gin engine with all routes registered, without
// listening on a port. It is used by createServer and by tests.
func setupRouter(w *Webserver) *gin.Engine {
	embeddedBuildFolder := newStaticFileSystem()
	// use debug mode if chekrr.debug is true
	if w.FullConfig.Bool("checkrr.debug") {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()
	err := router.SetTrustedProxies(w.trustedProxies)
	if err != nil {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "WebProxyFail",
		})
		checkrrLogger.Warn(message)
	}
	router.Use(static.Serve(w.BaseURL.String(), embeddedBuildFolder))
	api := router.Group(w.BaseURL.String() + "api")
	api.GET("/files/bad", getBadFiles)
	api.POST("/files/bad", deleteBadFiles)
	api.GET("/stats/current", getCurrentStats)
	api.GET("/stats/historical", getHistoricalStats)
	api.GET("/schedule", getSchedule)
	api.POST("/run", runCheckrr)
	return router
}

func getBadFiles(ctx *gin.Context) {
	var files []badFileData

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-files"))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			bad := check.BadFile{}
			err := json.Unmarshal(v, &bad)
			if err != nil {
				return err
			}
			badfiledata := badFileData{Path: string(k), Data: &bad}
			files = append(files, badfiledata)
		}
		return nil
	})
	if err != nil {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "DBAccessFail",
			TemplateData: map[string]interface{}{
				"Error": err.Error(),
			},
		})
		checkrrLogger.Fatal(message)
	}
	ctx.JSON(200, files)
}

// deleteBadFiles handles POST /api/files/bad. The request body is a JSON
// array of file paths (the stable keys of the bad-file records), not list
// indices, so deletions are immune to the list changing between requests.
// Paths that do not exist are ignored and reported back to the caller.
func deleteBadFiles(ctx *gin.Context) {
	var postData []string
	if err := ctx.BindJSON(&postData); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "request body must be a JSON array of file paths"})
		return
	}

	deleted, notFound, err := deleteBadFilesByPath(postData)
	if err != nil {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "DBAccessFail",
			TemplateData: map[string]interface{}{
				"Error": err.Error(),
			},
		})
		checkrrLogger.Error(message)
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"deleted": deleted, "notFound": notFound})
}

// deleteBadFilesByPath removes the bad-file records for the given paths in a
// single transaction. It returns the paths that were actually deleted and the
// paths that had no record (which are otherwise ignored). Running the whole
// delete in one bolt transaction keeps it atomic with respect to concurrent
// bad-file records written by an active check run.
func deleteBadFilesByPath(paths []string) (deleted []string, notFound []string, err error) {
	deleted = []string{}
	notFound = []string{}
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-files"))
		for _, path := range paths {
			if b.Get([]byte(path)) == nil {
				notFound = append(notFound, path)
				continue
			}
			if err := b.Delete([]byte(path)); err != nil {
				return err
			}
			deleted = append(deleted, path)
		}
		return nil
	})
	return deleted, notFound, err
}

func getCurrentStats(ctx *gin.Context) {
	var stats *Stats
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-stats"))
		statdata := b.Get([]byte("current-stats"))
		if len(statdata) != 0 {
			s := Stats{}
			err := json.Unmarshal(statdata, &s)
			if err != nil {
				return err
			}
			stats = &s
		}
		// The bad-file count is derived from the actual records in the same
		// transaction, so the reported count can never drift from the list.
		filesBucket := tx.Bucket([]byte("Checkrr-files"))
		if filesBucket != nil {
			if stats == nil {
				stats = &Stats{}
			}
			stats.BadFiles = uint64(filesBucket.Stats().KeyN)
		}
		return nil
	})
	if err != nil {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "DBAccessFail",
			TemplateData: map[string]interface{}{
				"Error": err.Error(),
			},
		})
		checkrrLogger.Fatal(message)
	}
	ctx.JSON(200, stats)
}

func getHistoricalStats(ctx *gin.Context) {
	var stats []statData
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-stats"))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			s := Stats{}
			err := json.Unmarshal(v, &s)
			if err != nil {
				return err
			}
			stat := statData{Timestamp: string(k), Data: &s}
			stats = append(stats, stat)
		}
		return nil
	})
	for len(stats) > 30 {
		_, stats = stats[0], stats[1:]
	}
	if err != nil {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "DBAccessFail",
			TemplateData: map[string]interface{}{
				"Error": err.Error(),
			},
		})
		checkrrLogger.Fatal(message)
	}
	ctx.JSON(200, stats)
}

func getSchedule(ctx *gin.Context) {
	if scheduler != nil {
		nextRun := scheduler.Entry(cronEntry).Next.UTC().String()
		ctx.JSON(200, nextRun)
	} else {
		ctx.JSON(200, nil)
	}
}

// runCheckrr handles POST /api/run. It goes through the same TryAcquire
// decision point as the cron scheduler: only one run executes at a time, and
// a trigger while a run is active gets an explicit 409 Conflict instead of
// being silently dropped.
func runCheckrr(ctx *gin.Context) {
	if !checkrrInstance.RunAsync() {
		message := localizer.MustLocalize(&i18n.LocalizeConfig{
			MessageID: "CheckMultiRunError",
		})
		ctx.JSON(http.StatusConflict, gin.H{
			"started": false,
			"running": true,
			"message": message,
		})
		return
	}
	ctx.JSON(http.StatusAccepted, gin.H{
		"started": true,
		"running": true,
	})
}

// file system code
type staticFileSystem struct {
	http.FileSystem
}

var _ static.ServeFileSystem = (*staticFileSystem)(nil)

func newStaticFileSystem() *staticFileSystem {
	sub, err := fs.Sub(staticFS, "build")

	if err != nil {
		panic(err)
	}

	return &staticFileSystem{
		FileSystem: http.FS(sub),
	}
}

func (s *staticFileSystem) Exists(prefix string, path string) bool {
	buildpath := ""
	if baseurl == "/" {
		buildpath = fmt.Sprintf("build%s", path)
	} else {
		buildpath = fmt.Sprintf("build/%s", strings.TrimPrefix(path, baseurl.String()))
	}

	// support for folders
	if strings.HasSuffix(path, "/") {
		_, err := staticFS.ReadDir(strings.TrimSuffix(buildpath, "/"))
		return err == nil
	}

	// support for files
	f, err := staticFS.Open(buildpath)
	if f != nil {
		_ = f.Close()
	}
	return err == nil
}

type statData struct {
	Timestamp string
	Data      *Stats
}

type badFileData struct {
	Path string
	Data *check.BadFile
}

type Stats struct {
	SonarrSubmissions uint64        `json:"sonarrSubmissions"`
	RadarrSubmissions uint64        `json:"radarrSubmissions"`
	LidarrSubmissions uint64        `json:"lidarrSubmissions"`
	FilesChecked      uint64        `json:"filesChecked"`
	HashMatches       uint64        `json:"hashMatches"`
	HashMismatches    uint64        `json:"hashMismatches"`
	VideoFiles        uint64        `json:"videoFiles"`
	AudioFiles        uint64        `json:"audioFiles"`
	UnknownFileCount  uint64        `json:"unknownFileCount"`
	NonVideo          uint64        `json:"nonVideo"`
	BadFiles          uint64        `json:"badFiles"`
	Running           bool          `json:"running"`
	Diff              time.Duration `json:"timeDiff"`
}
