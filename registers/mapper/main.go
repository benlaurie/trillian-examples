package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/google/trillian"
	"github.com/google/trillian-examples/registers/trillian_client"
	"google.golang.org/grpc"
)

var (
	trillianLog = flag.String("trillian_log", "localhost:8090", "address of the Trillian Log RPC server.")
	logID       = flag.Int64("log_id", 0, "Trillian LogID to read.")
	trillianMap = flag.String("trillian_map", "localhost:8095", "address of the Trillian Map RPC server.")
	mapID       = flag.Int64("map_id", 0, "Trillian MapID to write.")
)

type record struct {
	Entry map[string]interface{}
	Items []map[string]interface{}
}

// add only adds if the item is not already present in Items.
func (r *record) add(i map[string]interface{}) {
	for _, ii := range r.Items {
		if reflect.DeepEqual(i, ii) {
			return
		}
	}
	r.Items = append(r.Items, i)
}

type mapInfo struct {
	mapID int64
	tc    trillian.TrillianMapClient
	ctx   context.Context
}

func newInfo(tc trillian.TrillianMapClient, mapID int64, ctx context.Context) *mapInfo {
	i := &mapInfo{mapID: mapID, tc: tc, ctx: ctx}
	return i
}

func (i *mapInfo) createRecord(key string, entry map[string]interface{}, item map[string]interface{}) {
	ii := [1]map[string]interface{}{item}
	i.saveRecord(key, &record{Entry: entry, Items: ii[:]})
}

func (i *mapInfo) saveRecord(key string, value interface{}) {
	fmt.Printf("evicting %v -> %v\n", key, value)

	v, err := json.Marshal(value)
	if err != nil {
		log.Fatalf("Marshal() failed: %v", err)
	}

	hash := sha256.Sum256([]byte(key))
	l := trillian.MapLeaf{
		Index:     hash[:],
		LeafValue: v,
	}

	req := trillian.SetMapLeavesRequest{
		MapId:  i.mapID,
		Leaves: []*trillian.MapLeaf{&l},
	}

	if _, err = i.tc.SetLeaves(i.ctx, &req); err != nil {
		log.Fatalf("SetLeaves() failed: %v", err)
	}
}

func (i *mapInfo) getLeaf(key string) (*record, error) {
	hash := sha256.Sum256([]byte(key))
	index := [1][]byte{hash[:]}
	req := &trillian.GetMapLeavesRequest{
		MapId: i.mapID,
		Index: index[:],
	}

	resp, err := i.tc.GetLeaves(i.ctx, req)
	if err != nil {
		return nil, err
	}

	l := resp.MapLeafInclusion[0].Leaf.LeafValue
	log.Printf("key=%v leaf=%s", key, l)
	// FIXME: we should be able to detect non-existent vs. empty leaves
	if len(l) == 0 {
		return nil, nil
	}

	var r record
	err = json.Unmarshal(l, &r)
	if err != nil {
		return nil, err
	}

	return &r, nil
}

// Get the current record for the given key, possibly going to Trillian to look it up, possibly flushing the cache if needed.
func (i *mapInfo) get(key string) (*record, error) {
	r, err := i.getLeaf(key)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	return r, nil
}

type logScanner struct {
	info *mapInfo
}

func (s *logScanner) Leaf(leaf *trillian.LogLeaf) error {
	var l map[string]interface{}
	if err := json.Unmarshal(leaf.LeafValue, &l); err != nil {
		return err
	}

	e := l["Entry"].(map[string]interface{})
	t, err := time.Parse(time.RFC3339, e["entry-timestamp"].(string))
	if err != nil {
		return err
	}

	k := e["key"].(string)
	i := l["Item"].(map[string]interface{})
	log.Printf("k: %s ts: %s", k, t)

	cr, err := s.info.get(k)
	if err != nil {
		return err
	}
	if cr == nil {
		s.info.createRecord(k, e, i)
		return nil
	}

	ct, err := time.Parse(time.RFC3339, cr.Entry["entry-timestamp"].(string))
	if err != nil {
		return err
	}

	if t.Before(ct) {
		log.Printf("Skip")
		return nil
	} else if t.After(ct) {
		log.Printf("Replace")
		s.info.createRecord(k, e, i)
		return nil
	}

	log.Printf("Add")
	cr.add(i)
	s.info.saveRecord(k, cr)

	return nil
}

func main() {
	flag.Parse()

	tc := trillian_client.New(*trillianLog)
	defer tc.Close()

	g, err := grpc.Dial(*trillianMap, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Failed to dial Trillian Log: %v", err)
	}
	tmc := trillian.NewTrillianMapClient(g)

	i := newInfo(tmc, *mapID, context.Background())
	err = tc.Scan(*logID, &logScanner{info: i})
	if err != nil {
		log.Fatal(err)
	}
}
