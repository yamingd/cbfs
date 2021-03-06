package main

import (
	"encoding/json"
	"log"
	"strings"
)

type fileListing struct {
	Files map[string]interface{} `json:"files"`
	Dirs  map[string]interface{} `json:"dirs"`
	Path  string                 `json:"path"`
}

func toStringJoin(in []interface{}, sep string) string {
	s := []string{}
	for _, a := range in {
		s = append(s, a.(string))
	}
	return strings.Join(s, sep)
}

func listFiles(path string, includeMeta bool,
	depth int) (fileListing, error) {

	viewRes := struct {
		Rows []struct {
			Key   []interface{}
			Value map[string]interface{}
		}
	}{}

	// use the requested path to build our view query parameters
	startKey := []interface{}{}
	if path != "" {
		for _, k := range strings.Split(path, "/") {
			startKey = append(startKey, k)
		}
	}
	endKey := make([]interface{}, len(startKey)+1, len(startKey)+1)
	copy(endKey, startKey)
	endMarker := json.RawMessage([]byte{'{', '}'})
	endKey[len(startKey)] = &endMarker
	groupLevel := len(startKey) + depth

	// query the view
	err := couchbase.ViewCustom("cbfs", "file_browse",
		map[string]interface{}{
			"group_level": groupLevel,
			"start_key":   startKey,
			"end_key":     endKey,
		}, &viewRes)
	if err != nil {
		return fileListing{}, err
	}

	// use the view result to build a list of keys
	keys := make([]string, len(viewRes.Rows), len(viewRes.Rows))
	for i, r := range viewRes.Rows {
		keys[i] = toStringJoin(r.Key, "/")
	}

	// do a multi-get on the all the keys returned
	bulkResult := couchbase.GetBulk(keys)

	// divide items up into files and directories
	files := map[string]interface{}{}
	dirs := map[string]interface{}{}
	for _, r := range viewRes.Rows {
		key := toStringJoin(r.Key, "/")
		subkey := r.Key
		if len(r.Key) > depth {
			subkey = r.Key[len(r.Key)-depth:]
		}
		name := toStringJoin(subkey, "/")
		res, ok := bulkResult[key]
		if ok == true {
			// this means we have a file
			rv := map[string]interface{}{}
			err := json.Unmarshal(res.Body, &rv)
			if err != nil {
				log.Printf("Error deserializing json, ignoring: %v", err)
			} else {
				if includeMeta {
					files[name] = rv
				} else {
					files[name] = map[string]interface{}{}
				}
			}
		} else {
			// no record in the multi-get metans this is a directory
			dirs[name] = map[string]interface{}{
				"descendants": r.Value["count"],
				"size":        int64(r.Value["sum"].(float64)),
				"smallest":    int64(r.Value["min"].(float64)),
				"largest":     int64(r.Value["max"].(float64)),
			}
		}
	}

	rv := fileListing{
		Path:  "/" + path,
		Dirs:  dirs,
		Files: files,
	}

	return rv, nil
}
