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

package main

// Command docs are in cbtdoc.go.

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/cloud/bigtable"
)

var (
	// These get default values from $HOME/.cbtrc if it exists.
	project = flag.String("project", "", "project ID")
	zone    = flag.String("zone", "", "CBT zone")
	cluster = flag.String("cluster", "", "CBT cluster")
	creds   = flag.String("creds", "", "if set, use application credentials in this file")

	client      *bigtable.Client
	adminClient *bigtable.AdminClient
)

func getClient() *bigtable.Client {
	if client == nil {
		var err error
		client, err = bigtable.NewClient(context.Background(), *project, *zone, *cluster)
		if err != nil {
			log.Fatalf("Making bigtable.Client: %v", err)
		}
	}
	return client
}

func getAdminClient() *bigtable.AdminClient {
	if adminClient == nil {
		var err error
		adminClient, err = bigtable.NewAdminClient(context.Background(), *project, *zone, *cluster)
		if err != nil {
			log.Fatalf("Making bigtable.AdminClient: %v", err)
		}
	}
	return adminClient
}

func configFilename() string {
	// TODO(dsymonds): Might need tweaking for Windows.
	return filepath.Join(os.Getenv("HOME"), ".cbtrc")
}

func loadConfig() {
	filename := configFilename()
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		// silent fail if the file isn't there
		if os.IsNotExist(err) {
			return
		}
		log.Fatalf("Reading %s: %v", filename, err)
	}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		line := s.Text()
		i := strings.Index(line, "=")
		if i < 0 {
			log.Fatalf("Bad line in %s: %q", filename, line)
		}
		key, val := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		switch key {
		default:
			log.Fatalf("Unknown key in %s: %q", filename, key)
		case "project":
			*project = val
		case "zone":
			*zone = val
		case "cluster":
			*cluster = val
		case "creds":
			*creds = val
		}
	}
}

func main() {
	loadConfig()
	flag.Usage = usage
	flag.Parse()
	if *project == "" {
		log.Fatal("Missing -project")
	}
	if *zone == "" {
		log.Fatal("Missing -zone")
	}
	if *cluster == "" {
		log.Fatal("Missing -cluster")
	}
	if *creds != "" {
		os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", *creds)
	}
	if flag.NArg() == 0 {
		usage()
		os.Exit(1)
	}
	ctx := context.Background()
	for _, cmd := range commands {
		if cmd.Name == flag.Arg(0) {
			cmd.do(ctx, flag.Args()[1:]...)
			return
		}
	}
	log.Fatalf("Unknown command %q", flag.Arg(0))
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <command> ...\n", os.Args[0])
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\n%s", cmdSummary)
}

var cmdSummary string // generated in init, below

func init() {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 10, 8, 4, '\t', 0)
	for _, cmd := range commands {
		fmt.Fprintf(tw, "cbt %s\t%s\n", cmd.Name, cmd.Desc)
	}
	tw.Flush()
	buf.WriteString(configHelp)
	cmdSummary = buf.String()
}

var configHelp = `
For convenience, values of the -project, -zone, -cluster and -creds flags
may be specified in ` + configFilename() + ` in this format:
	project = my-project-123
	zone = us-central1-b
	cluster = my-cluster
	creds = path-to-account-key.json
All values are optional, and all will be overridden by flags.
`

var commands = []struct {
	Name, Desc string
	do         func(context.Context, ...string)
	Usage      string
}{
	{
		Name:  "createfamily",
		Desc:  "Create a column family",
		do:    doCreateFamily,
		Usage: "cbt createfamily <table> <family>",
	},
	{
		Name:  "createtable",
		Desc:  "Create a table",
		do:    doCreateTable,
		Usage: "cbt createtable <table>",
	},
	{
		Name:  "deletefamily",
		Desc:  "Delete a column family",
		do:    doDeleteFamily,
		Usage: "cbt deletefamily <table> <family>",
	},
	{
		Name:  "deleterow",
		Desc:  "Delete a row",
		do:    doDeleteRow,
		Usage: "cbt deleterow <table> <row>",
	},
	{
		Name:  "deletetable",
		Desc:  "Delete a table",
		do:    doDeleteTable,
		Usage: "cbt deletetable <table>",
	},
	{
		Name:  "doc",
		Desc:  "Print documentation for cbt",
		do:    doDoc,
		Usage: "cbt doc",
	},
	{
		Name:  "help",
		Desc:  "Print help text",
		do:    doHelp,
		Usage: "cbt help [command]",
	},
	{
		Name:  "lookup",
		Desc:  "Read from a single row",
		do:    doLookup,
		Usage: "cbt lookup <table> <row>",
	},
	{
		Name: "ls",
		Desc: "List tables and column families",
		do:   doLS,
		Usage: "cbt ls			List tables\n" +
			"cbt ls <table>		List column families in <table>",
	},
	{
		Name: "read",
		Desc: "Read rows",
		do:   doRead,
		Usage: "cbt read <table> [start=<row>] [limit=<row>] [prefix=<prefix>]\n" +
			"  start=<row>		Start reading at this row\n" +
			"  limit=<row>		Stop reading before this row\n" +
			"  prefix=<prefix>	Read rows with this prefix\n",
	},
	{
		Name: "set",
		Desc: "Set value of a cell",
		do:   doSet,
		Usage: "cbt set <table> <row> family:column=val[@ts] ...\n" +
			"  family:column=val[@ts] may be repeated to set multiple cells.\n" +
			"\n" +
			"  ts is an optional integer timestamp.\n" +
			"  If it cannot be parsed, the `@ts` part will be\n" +
			"  interpreted as part of the value.",
	},
}

func doCreateFamily(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatal("usage: cbt createfamily <table> <family>")
	}
	err := getAdminClient().CreateColumnFamily(ctx, args[0], args[1])
	if err != nil {
		log.Fatalf("Creating column family: %v", err)
	}
}

func doCreateTable(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatal("usage: cbt createtable <table>")
	}
	err := getAdminClient().CreateTable(ctx, args[0])
	if err != nil {
		log.Fatalf("Creating table: %v", err)
	}
}

func doDeleteFamily(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatal("usage: cbt deletefamily <table> <family>")
	}
	err := getAdminClient().DeleteColumnFamily(ctx, args[0], args[1])
	if err != nil {
		log.Fatalf("Deleting column family: %v", err)
	}
}

func doDeleteRow(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatal("usage: cbt deleterow <table> <row>")
	}
	tbl := getClient().Open(args[0])
	mut := bigtable.NewMutation()
	mut.DeleteRow()
	if err := tbl.Apply(ctx, args[1], mut); err != nil {
		log.Fatalf("Deleting row: %v", err)
	}
}

func doDeleteTable(ctx context.Context, args ...string) {
	if len(args) != 1 {
		log.Fatalf("Can't do `cbt deletetable %s`", args)
	}
	err := getAdminClient().DeleteTable(ctx, args[0])
	if err != nil {
		log.Fatalf("Deleting table: %v", err)
	}
}

// to break circular dependencies
var (
	doDocFn  func(ctx context.Context, args ...string)
	doHelpFn func(ctx context.Context, args ...string)
)

func init() {
	doDocFn = doDocReal
	doHelpFn = doHelpReal
}

func doDoc(ctx context.Context, args ...string)  { doDocFn(ctx, args...) }
func doHelp(ctx context.Context, args ...string) { doHelpFn(ctx, args...) }

func doDocReal(ctx context.Context, args ...string) {
	data := map[string]interface{}{
		"Commands": commands,
	}
	var buf bytes.Buffer
	if err := docTemplate.Execute(&buf, data); err != nil {
		log.Fatalf("Bad doc template: %v", err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatalf("Bad doc output: %v", err)
	}
	os.Stdout.Write(out)
}

var docTemplate = template.Must(template.New("doc").Funcs(template.FuncMap{
	"indent": func(s, ind string) string {
		ss := strings.Split(s, "\n")
		for i, p := range ss {
			ss[i] = ind + p
		}
		return strings.Join(ss, "\n")
	},
}).
	Parse(`
// DO NOT EDIT. THIS IS AUTOMATICALLY GENERATED.
// Run "cbt doc > cbtdoc.go" to regenerate.

/*
Cbt is a tool for doing basic interactions with Cloud Bigtable.

Usage:

	cbt [options] command [arguments]

The commands are:
{{range .Commands}}
	{{printf "%-25s %s" .Name .Desc}}{{end}}

Use "cbt help <command>" for more information about a command.

{{range .Commands}}
{{.Desc}}

Usage:
{{indent .Usage "\t"}}



{{end}}
*/
package main
`))

func doHelpReal(ctx context.Context, args ...string) {
	if len(args) == 0 {
		fmt.Print(cmdSummary)
		return
	}
	for _, cmd := range commands {
		if cmd.Name == args[0] {
			fmt.Println(cmd.Usage)
			return
		}
	}
	log.Fatalf("Don't know command %q", args[0])
}

func doLookup(ctx context.Context, args ...string) {
	if len(args) != 2 {
		log.Fatalf("usage: cbt lookup <table> <row>")
	}
	table, row := args[0], args[1]
	tbl := getClient().Open(table)
	r, err := tbl.ReadRow(ctx, row)
	if err != nil {
		log.Fatalf("Reading row: %v", err)
	}
	printRow(r)
}

func printRow(r bigtable.Row) {
	fmt.Println(strings.Repeat("-", 40))
	fmt.Println(r.Key())

	var fams []string
	for fam := range r {
		fams = append(fams, fam)
	}
	sort.Strings(fams)
	for _, fam := range fams {
		ris := r[fam]
		sort.Sort(byColumn(ris))
		for _, ri := range ris {
			ts := time.Unix(0, int64(ri.Timestamp)*1e3)
			fmt.Printf("  %-40s @ %s\n", ri.Column, ts.Format("2006/01/02-15:04:05.000000"))
			fmt.Printf("    %q\n", ri.Value)
		}
	}
}

type byColumn []bigtable.ReadItem

func (b byColumn) Len() int           { return len(b) }
func (b byColumn) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byColumn) Less(i, j int) bool { return b[i].Column < b[j].Column }

func doLS(ctx context.Context, args ...string) {
	switch len(args) {
	default:
		log.Fatalf("Can't do `cbt ls %s`", args)
	case 0:
		tables, err := getAdminClient().Tables(ctx)
		if err != nil {
			log.Fatalf("Getting list of tables: %v", err)
		}
		sort.Strings(tables)
		for _, table := range tables {
			fmt.Println(table)
		}
	case 1:
		table := args[0]
		ti, err := getAdminClient().TableInfo(ctx, table)
		if err != nil {
			log.Fatalf("Getting table info: %v", err)
		}
		sort.Strings(ti.Families)
		for _, fam := range ti.Families {
			fmt.Println(fam)
		}
	}
}

func doRead(ctx context.Context, args ...string) {
	if len(args) < 1 {
		log.Fatalf("usage: cbt read <table> [args ...]")
	}
	tbl := getClient().Open(args[0])

	parsed := make(map[string]string)
	for _, arg := range args[1:] {
		i := strings.Index(arg, "=")
		if i < 0 {
			log.Fatalf("Bad arg %q", arg)
		}
		key, val := arg[:i], arg[i+1:]
		switch key {
		default:
			log.Fatalf("Unknown arg key %q", key)
		case "start", "limit", "prefix":
			parsed[key] = val
		}
	}
	if (parsed["start"] != "" || parsed["limit"] != "") && parsed["prefix"] != "" {
		log.Fatal(`"start"/"limit" may not be mixed with "prefix"`)
	}

	var rr bigtable.RowRange
	if start, limit := parsed["start"], parsed["limit"]; limit != "" {
		rr = bigtable.NewRange(start, limit)
	} else if start != "" {
		rr = bigtable.InfiniteRange(start)
	}
	if prefix := parsed["prefix"]; prefix != "" {
		rr = bigtable.PrefixRange(prefix)
	}

	// TODO(dsymonds): Support filters.
	err := tbl.ReadRows(ctx, rr, func(r bigtable.Row) bool {
		printRow(r)
		return true
	})
	if err != nil {
		log.Fatalf("Reading rows: %v", err)
	}
}

var setArg = regexp.MustCompile(`([^:]+):([^=]*)=(.*)`)

func doSet(ctx context.Context, args ...string) {
	if len(args) < 3 {
		log.Fatalf("usage: cbt set <table> <row> family:[column]=val[@ts] ...")
	}
	tbl := getClient().Open(args[0])
	row := args[1]
	mut := bigtable.NewMutation()
	for _, arg := range args[2:] {
		m := setArg.FindStringSubmatch(arg)
		if m == nil {
			log.Fatalf("Bad set arg %q", arg)
		}
		val := m[3]
		ts := bigtable.Now()
		if i := strings.LastIndex(val, "@"); i >= 0 {
			// Try parsing a timestamp.
			n, err := strconv.ParseInt(val[i+1:], 0, 64)
			if err == nil {
				val = val[:i]
				ts = bigtable.Timestamp(n)
			}
		}
		mut.Set(m[1], m[2], ts, []byte(val))
	}
	if err := tbl.Apply(ctx, row, mut); err != nil {
		log.Fatalf("Applying mutation: %v", err)
	}
}
