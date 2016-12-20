package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/oauth2/google"

	"cloud.google.com/go/datastore"

	uuid "github.com/satori/go.uuid"

	taskqueue "google.golang.org/api/taskqueue/v1beta2"
)

const (
	reqQueue = "hb-req"
	resQueue = "hb-res"
)

type multiError []error

// Error returns error string
func (me multiError) Error() string {
	var buf bytes.Buffer
	for _, err := range me {
		buf.WriteString(err.Error())
		buf.WriteString("\n")
	}
	return buf.String()
}

// TestPkg represents test package
type TestPkg struct {
	Name      string
	Dir       string
	TestFiles map[string]*TestFile
}

// TestFile represents test file
type TestFile struct {
	Name      string
	TestFuncs []string
}

// Request represents test request.
type Request struct {
	ID     string
	Commit string
	Pkg    string
	File   string
	Func   string
}

// TestResult represents a test result.
type TestResult struct {
	ID        string
	Commit    string
	Pkg       string
	File      string
	Func      string
	Output    string `datastore:",noindex"`
	Error     string `datastore:",noindex"`
	CreatedAt time.Time
}

// NewTestResultKey returns TestResult key
func NewTestResultKey(id, commit, pkg, f string) *datastore.Key {
	keyName := fmt.Sprintf("%s:%s:%s:%s", id, commit, pkg, f)
	return datastore.NameKey("TestResult", keyName, nil)
}

var alltests map[string]*TestPkg

var startTime int64

var project string

var tqService *taskqueue.Service
var dsClient *datastore.Client

func init() {
	project = os.Getenv("HB_GCP_PROJECT")
	if project == "" {
		log.Fatalf("need to set environment variable:HB_GCP_PROJECT\n")
	}

	ctx := context.Background()

	client, err := google.DefaultClient(ctx, taskqueue.TaskqueueScope, taskqueue.TaskqueueConsumerScope)
	if err != nil {
		log.Fatalf("failed to create default google client. err:%v", err)
	}

	if s, err := taskqueue.New(client); err != nil {
		log.Fatalf("failed to create taskqueue service. err:%v", err)
	} else {
		tqService = s
	}

	if client, err := datastore.NewClient(ctx, project); err != nil {
		log.Fatalf("failed to create datastore client. err:%v", err)
	} else {
		dsClient = client
	}
}

func main() {

	startTime = time.Now().UnixNano()

	if len(os.Args) < 2 {
		log.Fatal("usage: hb-client <commit>")
	}

	commit := os.Args[1]

	if err := parseTestPkgs(); err != nil {
		log.Fatalf("parse tests failed: %v", err)
	}

	chs := make([]chan error, 0, 32)

	id := uuid.NewV4().String()

	for pkgname, pkg := range alltests {
		for filename, tfile := range pkg.TestFiles {
			for _, f := range tfile.TestFuncs {
				ch := doRequest(id, commit, pkgname, filename, f)
				chs = append(chs, ch)
			}
		}
	}

	if err := waitForErrors(chs); err != nil {
		log.Fatalf("request failed: %v", err)
	}

	if err := waitForResults(id, commit); err != nil {
		log.Fatalf("check result failed: %v", err)
	}
}

func parseTestPkgs() error {
	alltests = make(map[string]*TestPkg)

	reader := bufio.NewReader(os.Stdin)

	for {
		line, isPrefix, err := reader.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read line failed: %v", err)
		}
		if isPrefix {
			return fmt.Errorf("unexpected too line.")
		}

		s := strings.Split(string(line), ":")
		pkgname := s[0]
		pkgdir := s[1]
		files := make([]string, 0, 32)
		for _, f := range strings.Split(s[2], ",") {
			if f != "" {
				files = append(files, f)
			}
		}

		pkg, ok := alltests[pkgname]
		if !ok {
			pkg = &TestPkg{
				Name:      pkgname,
				Dir:       pkgdir,
				TestFiles: make(map[string]*TestFile),
			}
			alltests[pkgname] = pkg
		}

		for _, f := range files {
			tfile := &TestFile{Name: f}
			tfile.TestFuncs, err = parseTestFuncs(path.Join(pkgdir, f))
			if err != nil {
				return fmt.Errorf("go file parse error: %v", err)
			}
			pkg.TestFiles[f] = tfile
		}
	}

	return nil
}

var testFileSet = token.NewFileSet()

func parseTestFuncs(filename string) ([]string, error) {

	f, err := parser.ParseFile(testFileSet, filename, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	testFuncs := make([]string, 0, 8)

	for _, d := range f.Decls {
		n, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if n.Recv != nil {
			continue
		}
		name := n.Name.String()
		if isTest(name) {
			testFuncs = append(testFuncs, name)
		}
	}

	return testFuncs, nil
}

func doRequest(id, commit, pkg, file, f string) chan error {
	ch := make(chan error)
	go func() {
		ch <- func() error {
			req := Request{
				ID:     id,
				Commit: commit,
				Pkg:    pkg,
				File:   file,
				Func:   f,
			}

			if err := insertRequest(&req); err != nil {
				return err
			}
			return nil
		}()
	}()
	return ch
}

func isTest(name string) bool {
	if name == "TestMain" {
		return false
	}
	prefix := "Test"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	rune, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(rune)
}

func insertRequest(req *Request) error {

	tasksService := taskqueue.NewTasksService(tqService)

	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	b64 := base64.StdEncoding.EncodeToString(data)

	task := taskqueue.Task{
		PayloadBase64: b64,
		QueueName:     reqQueue,
		Tag:           req.ID,
	}

	call := tasksService.Insert("s~"+project, reqQueue, &task)

	_, err = call.Do()
	if err != nil {
		return err
	}

	return nil
}

func waitForErrors(chs []chan error) error {
	var merr multiError
	for _, ch := range chs {
		if err := <-ch; err != nil {
			merr = append(merr, err)
		}
	}
	if len(merr) > 0 {
		return merr
	}
	return nil
}

func waitForResults(id, commit string) error {
	keys := make([]*datastore.Key, 0, 100)

	for pkgname, pkg := range alltests {
		for _, file := range pkg.TestFiles {
			for _, f := range file.TestFuncs {
				key := NewTestResultKey(id, commit, pkgname, f)
				keys = append(keys, key)
			}
		}
	}

	results := make(map[string]*TestResult)

	errorNum := 0

	retryCount := 30 * 60 // 30 minutes
	for i := 0; i < retryCount; i++ {
		rs, err := getResults(id)
		if err != nil {
			return fmt.Errorf("get result failed: %v", err)
		}

		for _, r := range rs {
			key := NewTestResultKey(r.ID, r.Commit, r.Pkg, r.Func)
			if r.Error != "" {
				errorNum++
			}
			results[key.Encode()] = r
		}

		fmt.Printf("\rprocessing... %4d/%d (fail:%d) %dsec",
			len(results), len(keys), errorNum, (time.Now().UnixNano()-startTime)/1000000000)

		if len(results) == len(keys) {
			break
		}

		time.Sleep(1 * time.Second)
	}

	// print results
	fmt.Println("\n============= result =============")

	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		result := results[key.Encode()]
		if result == nil {
			continue
		}
		if result.Error != "" {
			line := fmt.Sprintf("%s/%s:%s - %s %s", result.Pkg, result.File, result.Func, result.Output, result.Error)
			lines = append(lines, line)
		}
	}

	sort.Strings(lines)

	for _, line := range lines {
		fmt.Println(line)
	}

	if len(keys) != len(results) {
		return fmt.Errorf("timeout %4d/%d (fail:%d)", len(results), len(keys), errorNum)
	}

	return nil
}

func getResults(id string) ([]*TestResult, error) {
	tasksService := taskqueue.NewTasksService(tqService)

	tasks, err := tasksService.Lease("s~"+project, resQueue, 100, 60).GroupByTag(true).Tag(id).Do()
	if err != nil {
		// only log and retry
		fmt.Fprintf(os.Stderr, "lease task failed: %v", err)
		time.Sleep(10 * time.Second)
		return nil, nil
	}

	if len(tasks.Items) == 0 {
		return nil, nil
	}

	var mutex sync.Mutex
	var wg sync.WaitGroup

	results := make([]*TestResult, 0, len(tasks.Items))
	for _, task := range tasks.Items {
		task := task

		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := tasksService.Delete("s~"+project, resQueue, task.Id).Do(); err != nil {
				fmt.Fprintf(os.Stderr, "delete task failed: %v\n", err)
				return
			}

			payload, err := base64.StdEncoding.DecodeString(task.PayloadBase64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "decode base64 result: %v\n", err)
				return
			}

			var result TestResult
			if err := json.Unmarshal(payload, &result); err != nil {
				fmt.Fprintf(os.Stderr, "unmarshal json result: %v\n", err)
				return
			}

			mutex.Lock()
			defer mutex.Unlock()
			results = append(results, &result)
		}()
	}

	wg.Wait()

	return results, nil
}

// 処理結果をDatastoreより取り出す
// Datastore ClientがDefferred keyでerrorを返す為、別方式に変更
// https://github.com/GoogleCloudPlatform/google-cloud-go/blob/3c4c8cc11d151d76587802cb55dd7b80beca832b/datastore/datastore.go#L411
func _waitForResults(id, commit string) error {
	ctx := context.Background()

	keys := make([]*datastore.Key, 0, 100)

	for pkgname, pkg := range alltests {
		for _, file := range pkg.TestFiles {
			for _, f := range file.TestFuncs {
				key := NewTestResultKey(id, commit, pkgname, f)
				keys = append(keys, key)
			}
		}
	}

	results := make(map[string]*TestResult)

	errorNum := 0

	retryCount := 60 * 60 // 1 hour

	tmpKeys := keys[:]
	for i := 0; i < retryCount; i++ {
		if len(tmpKeys) == 0 {
			break
		}
		tmpResults := make([]TestResult, len(tmpKeys))
		if err := dsClient.GetMulti(ctx, tmpKeys, tmpResults); err == nil {
			for j, key := range tmpKeys {
				result := tmpResults[j]
				results[key.Encode()] = &result
				if result.Error != "" {
					errorNum++
				}
			}
			fmt.Printf("\rprocessing... %4d/%d (fail:%d)", len(results), len(keys), errorNum)
			fmt.Printf("\ndone! time: %d millisec\n", (time.Now().UnixNano()-startTime)/1000000)
			break
		} else if merr, ok := err.(datastore.MultiError); ok {
			for j := len(tmpKeys) - 1; j >= 0; j-- {
				if merr[j] == nil {
					result := tmpResults[j]
					results[tmpKeys[j].Encode()] = &result
					if result.Error != "" {
						errorNum++
					}

					// remove key
					tmpKeys = append(tmpKeys[:j], tmpKeys[j+1:]...)
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "get result failed: %v\n", err)
		}

		fmt.Printf("\rprocessing... %4d/%d (fail:%d) %dsec",
			len(results), len(keys), errorNum, (time.Now().UnixNano()-startTime)/1000000000)
		time.Sleep(1 * time.Second)
	}

	// print results
	fmt.Println("\n============= result =============")
	for _, key := range keys {
		result := results[key.Encode()]
		if result == nil {
			continue
		}
		if result.Error != "" {
			fmt.Printf("%s/%s:%s - %s %s\n", result.Pkg, result.File, result.Func, result.Output, result.Error)
		}
	}

	if len(keys) != len(results) {
		return fmt.Errorf("timeout %4d/%d (fail:%d)", len(results), len(keys), errorNum)
	}

	if errorNum == 0 {
		fmt.Println("All passed.")
	}

	return nil
}
