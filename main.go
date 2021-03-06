package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/couchbaselabs/cbfs/config"
	"github.com/dustin/go-humanize"
	"github.com/dustin/gomemcached"
)

var bindAddr = flag.String("bind", ":8484", "Address to bind web thing to")
var root = flag.String("root", "storage", "Storage location")
var couchbaseServer = flag.String("couchbase", "", "Couchbase URL")
var couchbaseBucket = flag.String("bucket", "default", "Couchbase bucket")
var cachePercentage = flag.Int("cachePercent", 100,
	"Percentage of proxied requests to eagerly cache.")
var enableViewProxy = flag.Bool("viewProxy", false,
	"Enable the view proxy")
var enableCRUDProxy = flag.Bool("crudProxy", false,
	"Enable the CRUD proxy")
var verbose = flag.Bool("verbose", false, "Show some more stuff")
var readTimeout = flag.Duration("serverTimeout", 5*time.Minute,
	"Web server read timeout")
var viewTimeout = flag.Duration("viewTimeout", 5*time.Second,
	"Couchbase view client read timeout")
var internodeTimeout = flag.Duration("internodeTimeout", 5*time.Second,
	"Internode client read timeout")
var useSyslog = flag.Bool("syslog", false, "Log to syslog")

var globalConfig *cbfsconfig.CBFSConfig

func init() {
	conf := cbfsconfig.DefaultConfig()
	globalConfig = &conf
}

type prevMeta struct {
	Headers  http.Header `json:"headers"`
	OID      string      `json:"oid"`
	Length   int64       `json:"length"`
	Modified time.Time   `json:"modified"`
	Revno    int         `json:"revno"`
}

type fileMeta struct {
	Headers  http.Header      `json:"headers"`
	OID      string           `json:"oid"`
	Length   int64            `json:"length"`
	Userdata *json.RawMessage `json:"userdata,omitempty"`
	Modified time.Time        `json:"modified"`
	Previous []prevMeta       `json:"older"`
	Revno    int              `json:"revno"`
	Type     string           `json:"type"`
}

func (fm fileMeta) MarshalJSON() ([]byte, error) {
	m := map[string]interface{}{
		"oid":      fm.OID,
		"headers":  map[string][]string(fm.Headers),
		"type":     "file",
		"ctype":    fm.Headers.Get("Content-Type"),
		"length":   fm.Length,
		"modified": fm.Modified,
		"revno":    fm.Revno,
	}

	if fm.Userdata != nil {
		m["userdata"] = fm.Userdata
	}
	if len(fm.Previous) > 0 {
		m["older"] = fm.Previous
	}
	return json.Marshal(m)
}

func mustEncode(i interface{}) []byte {
	rv, err := json.Marshal(i)
	if err != nil {
		log.Panicf("Error mustEncoding %#v: %v", i, err)
	}
	return rv
}

func storeMeta(k string, fm fileMeta, revs int) error {
	return couchbase.Update(k, 0, func(in []byte) ([]byte, error) {
		existing := fileMeta{}
		err := json.Unmarshal(in, &existing)
		if err == nil {
			fm.Userdata = existing.Userdata
			fm.Revno = existing.Revno + 1

			if revs == -1 || revs > 0 {
				newMeta := prevMeta{
					Headers:  existing.Headers,
					OID:      existing.OID,
					Length:   existing.Length,
					Modified: existing.Modified,
					Revno:    existing.Revno,
				}

				fm.Previous = append(existing.Previous,
					newMeta)

				diff := len(fm.Previous) - revs
				if revs != -1 && diff > 0 {
					fm.Previous = fm.Previous[diff:]
				}
			}
		}
		return json.Marshal(fm)
	})
}

func main() {
	flag.Parse()

	initLogger(*useSyslog)
	initNodeListKeys()

	http.DefaultTransport = TimeoutTransport(*internodeTimeout)

	if getHash() == nil {
		fmt.Fprintf(os.Stderr,
			"Unsupported hash specified: %v.  Supported hashes:\n",
			globalConfig.Hash)
		for h := range hashBuilders {
			fmt.Fprintf(os.Stderr, " * %v\n", h)
		}
		os.Exit(1)
	}

	err := initServerId()
	if err != nil {
		log.Fatalf("Error initializing server ID: %v", err)
	}

	if *maxStorageString != "" {
		maxStorage, err = humanize.ParseBytes(*maxStorageString)
		if err != nil {
			log.Fatalf("Error parsing max storage parameter: %v",
				err)
		}
	}

	couchbase, err = dbConnect()
	if err != nil {
		log.Fatalf("Can't connect to couchbase: %v", err)
	}

	if err = os.MkdirAll(*root, 0777); err != nil {
		log.Fatalf("Couldn't create storage dir: %v", err)
	}

	err = updateConfig()
	if err != nil && !gomemcached.IsNotFound(err) {
		log.Printf("Error updating initial config, using default: %v",
			err)
	}
	if *verbose {
		log.Printf("Server config:")
		globalConfig.Dump(os.Stdout)
	}

	go reloadConfig()

	go dnsServices()

	internodeTaskQueue = make(chan internodeTask, *taskWorkers*1024)
	initTaskQueueWorkers()

	go heartbeat()
	go startTasks()

	time.AfterFunc(time.Second*time.Duration(rand.Intn(30)+5), grabSomeData)

	go serveFrame()

	s := &http.Server{
		Addr:        *bindAddr,
		Handler:     http.HandlerFunc(httpHandler),
		ReadTimeout: *readTimeout,
	}
	log.Printf("Listening to web requests on %s as server %s",
		*bindAddr, serverId)
	log.Fatal(s.ListenAndServe())
}
