package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"golang.org/x/oauth2/google"

	"cloud.google.com/go/datastore"

	taskqueue "google.golang.org/api/taskqueue/v1beta2"
)

const (
	queue = "hb-req"
)

var lastCommit string

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
	Output    string
	Error     string
	CreatedAt time.Time
}

// NewTestResultKey returns TestResult key
func NewTestResultKey(id, commit, pkg, f string) *datastore.Key {
	keyName := fmt.Sprintf("%s:%s:%s:%s", id, commit, pkg, f)
	return datastore.NameKey("TestResult", keyName, nil)
}

var tqService *taskqueue.Service
var dsClient *datastore.Client

var project string
var appDir string

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

	appDir = os.Getenv("HB_APP")
	if appDir == "" {
		log.Fatalf("need to set environment variable:HB_APP\n")
	}
}

func main() {

	ctx := context.Background()

	for {
		req, err := getRequest("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "getRequest failed: %v\n", err)
			time.Sleep(10 * time.Second)
			continue
		}
		if req == nil {
			fmt.Print(".")
			time.Sleep(1 * time.Second)
			continue
		}

		fmt.Printf("request found: %+v\n", req)

		output, err := runTest(req)

		fmt.Printf("test output: %s\n", output)

		result := TestResult{
			ID:        req.ID,
			Commit:    req.Commit,
			Pkg:       req.Pkg,
			File:      req.File,
			Func:      req.Func,
			Output:    output,
			CreatedAt: time.Now(),
		}

		if err != nil {
			result.Error = err.Error()
		}

		key := NewTestResultKey(req.ID, req.Commit, req.Pkg, req.Func)

		_, err = dsClient.Put(ctx, key, &result)
		if err != nil {
			fmt.Fprintf(os.Stderr, "save result failed: %s\n", err.Error())
			time.Sleep(10 * time.Second)
			continue
		}
	}
}

func runTest(req *Request) (output string, err error) {
	if req.Commit != lastCommit {
		if err := checkoutCode(req.Commit); err != nil {
			lastCommit = "" // just in case
			return "", fmt.Errorf("git checkout failed: %v", err)
		}
		lastCommit = req.Commit
	}

	cmd := exec.Command("bash", "test.sh")
	cmd.Env = append(os.Environ(),
		"HB_COMMIT="+req.Commit,
		"HB_PKG="+req.Pkg,
		"HB_FUNC="+req.Func)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}

	return buf.String(), nil
}

func getRequest(tag string) (*Request, error) {

	tasksService := taskqueue.NewTasksService(tqService)

	call := tasksService.Lease("s~"+project, queue, 1, 60)

	if tag != "" {
		call = call.GroupByTag(true).Tag(tag)
	}

	tasks, err := call.Do()
	if err != nil {
		return nil, err
	}

	if len(tasks.Items) == 0 {
		return nil, nil
	}

	if len(tasks.Items) > 1 {
		return nil, fmt.Errorf("unexpected num of tasks:%d", len(tasks.Items))
	}

	task := tasks.Items[0]

	if err := tasksService.Delete("s~"+project, queue, task.Id).Do(); err != nil {
		return nil, err
	}

	payload, err := base64.StdEncoding.DecodeString(task.PayloadBase64)
	if err != nil {
		return nil, err
	}

	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}

	return &req, nil
}

func checkoutCode(commit string) error {
	fmt.Println("git checkout " + commit)

	cmd := exec.Command("git", "checkout", commit)

	cmd.Dir = appDir

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "git checkout failed: %s\n", buf.String())
		return err
	}

	return nil
}
