/*
Copyright 2015 Google Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bigtable

import (
	"flag"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/cloud/bigtable/bttest"
	btspb "google.golang.org/cloud/bigtable/internal/service_proto"
)

func dataChunk(fam, col string, ts int64, data string) string {
	return fmt.Sprintf("chunks:<row_contents:<name:%q columns:<qualifier:%q cells:<timestamp_micros:%d value:%q>>>>", fam, col, ts, data)
}

func commit() string { return "chunks:<commit_row:true>" }

var chunkTests = []struct {
	desc   string
	chunks []string // sequence of ReadRowsResponse protos in text format
	want   map[string]Row
}{
	{
		desc: "single row single chunk",
		chunks: []string{
			`row_key: "row1" ` + dataChunk("fam", "col1", 1428382701000000, "data") + commit(),
		},
		want: map[string]Row{
			"row1": Row{
				"fam": []ReadItem{{
					Row:       "row1",
					Column:    "fam:col1",
					Timestamp: 1428382701000000,
					Value:     []byte("data"),
				}},
			},
		},
	},
	{
		desc: "single row multiple chunks",
		chunks: []string{
			`row_key: "row1" ` + dataChunk("fam", "col1", 1428382701000000, "data"),
			`row_key: "row1" ` + dataChunk("fam", "col2", 1428382702000000, "more data"),
			`row_key: "row1" ` + commit(),
		},
		want: map[string]Row{
			"row1": Row{
				"fam": []ReadItem{
					{
						Row:       "row1",
						Column:    "fam:col1",
						Timestamp: 1428382701000000,
						Value:     []byte("data"),
					},
					{
						Row:       "row1",
						Column:    "fam:col2",
						Timestamp: 1428382702000000,
						Value:     []byte("more data"),
					},
				},
			},
		},
	},
	// TODO(dsymonds): More test cases, including
	//	- multiple rows
	//	- reset_row
}

func TestChunkReader(t *testing.T) {
	for _, tc := range chunkTests {
		cr := new(chunkReader)
		got := make(map[string]Row)
		for i, txt := range tc.chunks {
			rrr := new(btspb.ReadRowsResponse)
			if err := proto.UnmarshalText(txt, rrr); err != nil {
				t.Fatalf("%s: internal error: bad #%d test text: %v", tc.desc, i, err)
			}
			if row := cr.process(rrr); row != nil {
				got[row.Key()] = row
			}
		}
		// TODO(dsymonds): check for partial rows?
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: processed response mismatch.\n got %+v\nwant %+v", tc.desc, got, tc.want)
		}
	}
}

func TestPrefix(t *testing.T) {
	tests := []struct {
		prefix, succ string
	}{
		{"", ""},
		{"\xff", ""}, // when used, "" means Infinity
		{"x\xff", "y"},
		{"\xfe", "\xff"},
	}
	for _, tc := range tests {
		got := prefixSuccessor(tc.prefix)
		if got != tc.succ {
			t.Errorf("prefixSuccessor(%q) = %q, want %s", tc.prefix, got, tc.succ)
			continue
		}
		r := PrefixRange(tc.prefix)
		if tc.succ == "" && r.limit != "" {
			t.Errorf("PrefixRange(%q) got limit %q", tc.prefix, r.limit)
		}
		if tc.succ != "" && r.limit != tc.succ {
			t.Errorf("PrefixRange(%q) got limit %q, want %q", tc.prefix, r.limit, tc.succ)
		}
	}
}

var useProd = flag.String("use_prod", "", `if set to "proj,zone,cluster,table", run integration test against production`)

func TestClientIntegration(t *testing.T) {
	proj, zone, cluster, table := "proj", "zone", "cluster", "mytable"
	var clientOpts []ClientOption
	timeout := 10 * time.Second
	if *useProd == "" {
		srv, err := bttest.NewServer()
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		t.Logf("bttest.Server running on %s", srv.Addr)
		clientOpts = []ClientOption{WithCredentials(nil), WithInsecureAddr(srv.Addr)}
	} else {
		t.Logf("Running test against production")
		a := strings.Split(*useProd, ",")
		proj, zone, cluster, table = a[0], a[1], a[2], a[3]
		timeout = 1 * time.Minute
	}

	ctx, _ := context.WithTimeout(context.Background(), timeout)

	client, err := NewClient(ctx, proj, zone, cluster, clientOpts...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	adminClient, err := NewAdminClient(ctx, proj, zone, cluster, clientOpts...)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer adminClient.Close()

	// Delete the table at the end of the test.
	// Do this even before creating the table so that if this is running
	// against production and CreateTable fails there's a chance of cleaning it up.
	defer adminClient.DeleteTable(ctx, table)

	if err := adminClient.CreateTable(ctx, table); err != nil {
		t.Fatalf("Creating table: %v", err)
	}
	if err := adminClient.CreateColumnFamily(ctx, table, "follows"); err != nil {
		t.Fatalf("Creating column family: %v", err)
	}

	tbl := client.Open(table)

	// Insert some data.
	initialData := map[string][]string{
		"wmckinley":   []string{"tjefferson"},
		"gwashington": []string{"jadams"},
		"tjefferson":  []string{"gwashington", "jadams", "wmckinley"},
		"jadams":      []string{"gwashington", "tjefferson"},
	}
	for row, ss := range initialData {
		mut := NewMutation()
		for _, name := range ss {
			mut.Set("follows", name, 0, []byte("1"))
		}
		if err := tbl.Apply(ctx, row, mut); err != nil {
			t.Errorf("Mutating row %q: %v", row, err)
		}
	}

	// Fetch a row.
	row, err := tbl.ReadRow(ctx, "jadams")
	if err != nil {
		t.Fatalf("Reading a row: %v", err)
	}
	wantRow := Row{
		"follows": []ReadItem{
			{Row: "jadams", Column: "follows:gwashington", Value: []byte("1")},
			{Row: "jadams", Column: "follows:tjefferson", Value: []byte("1")},
		},
	}
	for _, ris := range row {
		sort.Sort(byColumn(ris))
	}
	if !reflect.DeepEqual(row, wantRow) {
		t.Errorf("Read row mismatch.\n got %#v\nwant %#v", row, wantRow)
	}

	// Do a bunch of reads with filters.
	readTests := []struct {
		desc   string
		rr     RowRange
		filter Filter // may be nil

		// We do the read, grab all the cells, turn them into "<row>-<col>-<val>",
		// sort that list, and join with a comma.
		want string
	}{
		{
			desc: "read all, unfiltered",
			rr:   RowRange{},
			want: "gwashington-jadams-1,jadams-gwashington-1,jadams-tjefferson-1,tjefferson-gwashington-1,tjefferson-jadams-1,tjefferson-wmckinley-1,wmckinley-tjefferson-1",
		},
		{
			desc: "read with InfiniteRange, unfiltered",
			rr:   InfiniteRange("tjefferson"),
			want: "tjefferson-gwashington-1,tjefferson-jadams-1,tjefferson-wmckinley-1,wmckinley-tjefferson-1",
		},
		{
			desc: "read with NewRange, unfiltered",
			rr:   NewRange("gargamel", "hubbard"),
			want: "gwashington-jadams-1",
		},
		{
			desc: "read with PrefixRange, unfiltered",
			rr:   PrefixRange("jad"),
			want: "jadams-gwashington-1,jadams-tjefferson-1",
		},
		{
			desc: "read with SingleRow, unfiltered",
			rr:   SingleRow("wmckinley"),
			want: "wmckinley-tjefferson-1",
		},
		{
			desc:   "read all, with ColumnFilter",
			rr:     RowRange{},
			filter: ColumnFilter(".*j.*"), // matches "jadams" and "tjefferson"
			want:   "gwashington-jadams-1,jadams-tjefferson-1,tjefferson-jadams-1,wmckinley-tjefferson-1",
		},
	}
	for _, tc := range readTests {
		var opts []ReadOption
		if tc.filter != nil {
			opts = append(opts, RowFilter(tc.filter))
		}
		var elt []string
		err := tbl.ReadRows(context.Background(), tc.rr, func(r Row) bool {
			for _, ris := range r {
				for _, ri := range ris {
					// Use the column qualifier only to make the test data briefer.
					col := ri.Column[strings.Index(ri.Column, ":")+1:]
					x := fmt.Sprintf("%s-%s-%s", ri.Row, col, ri.Value)
					elt = append(elt, x)
				}
			}
			return true
		}, opts...)
		if err != nil {
			t.Errorf("%s: %v", tc.desc, err)
			continue
		}
		sort.Strings(elt)
		if got := strings.Join(elt, ","); got != tc.want {
			t.Errorf("%s: wrong reads.\n got %q\nwant %q", tc.desc, got, tc.want)
		}
	}

	// Check ReadModifyWrite.

	if err := adminClient.CreateColumnFamily(ctx, table, "counter"); err != nil {
		t.Fatalf("Creating column family: %v", err)
	}

	appendRMW := func(b []byte) *ReadModifyWrite {
		rmw := NewReadModifyWrite()
		rmw.AppendValue("counter", "likes", b)
		return rmw
	}
	incRMW := func(n int64) *ReadModifyWrite {
		rmw := NewReadModifyWrite()
		rmw.Increment("counter", "likes", n)
		return rmw
	}
	rmwSeq := []struct {
		desc string
		rmw  *ReadModifyWrite
		want []byte
	}{
		{
			desc: "append #1",
			rmw:  appendRMW([]byte{0, 0, 0}),
			want: []byte{0, 0, 0},
		},
		{
			desc: "append #2",
			rmw:  appendRMW([]byte{0, 0, 0, 0, 17}), // the remaining 40 bits to make a big-endian 17
			want: []byte{0, 0, 0, 0, 0, 0, 0, 17},
		},
		{
			desc: "increment",
			rmw:  incRMW(8),
			want: []byte{0, 0, 0, 0, 0, 0, 0, 25},
		},
	}
	for _, step := range rmwSeq {
		row, err := tbl.ApplyReadModifyWrite(ctx, "gwashington", step.rmw)
		if err != nil {
			t.Fatalf("ApplyReadModifyWrite %+v: %v", step.rmw, err)
		}
		clearTimestamps(row)
		wantRow := Row{"counter": []ReadItem{{Row: "gwashington", Column: "counter:likes", Value: step.want}}}
		if !reflect.DeepEqual(row, wantRow) {
			t.Fatalf("After %s,\n got %v\nwant %v", step.desc, row, wantRow)
		}
	}
}

type byColumn []ReadItem

func (b byColumn) Len() int           { return len(b) }
func (b byColumn) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byColumn) Less(i, j int) bool { return b[i].Column < b[j].Column }

func clearTimestamps(r Row) {
	for _, ris := range r {
		for i := range ris {
			ris[i].Timestamp = 0
		}
	}
}
