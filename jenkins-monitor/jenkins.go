// Copyright (c) 2026 Aaron LI
//
// Jenkins REST API client (read-only).
//
// Co-authored-by: Deepseek-v4-flash (wit Pi Coding Agent)
//

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var errNotFound = errors.New("jenkins: not found")

// jenkinsBuild is the interesting subset of a Jenkins build/run.
type jenkinsBuild struct {
	Number    int64  `json:"number"`
	Result    string `json:"result"` // SUCCESS/FAILURE/UNSTABLE/ABORTED/NOT_BUILT; "" while running
	Timestamp int64  `json:"timestamp"`
	URL       string `json:"url"`
}

// jenkinsJob is the interesting subset of a Jenkins job.
type jenkinsJob struct {
	Color              string        `json:"color"`
	LastBuild          *jenkinsBuild `json:"lastBuild"`
	LastCompletedBuild *jenkinsBuild `json:"lastCompletedBuild"`
}

// jenkinsNode is one Jenkins computer (execution node).
type jenkinsNode struct {
	DisplayName        string `json:"displayName"`
	Offline            bool   `json:"offline"`
	OfflineCauseReason string `json:"offlineCauseReason"`
}

type jenkinsClient struct {
	baseURL string
	user    string
	secret  string // password or API token
	client  *http.Client
}

func newJenkinsClient(cfg *ConfigJenkins) *jenkinsClient {
	secret := cfg.APIToken
	if secret == "" {
		secret = cfg.Password
	}
	return &jenkinsClient{
		baseURL: strings.TrimRight(cfg.URL, "/"),
		user:    cfg.User,
		secret:  secret,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// jobPath returns the URL of a job's API endpoint, e.g.
// "https://ci/…/job/DragonFlyBSD/api/json?tree=…"
func (c *jenkinsClient) jobPath(name, tree string) string {
	query := url.Values{"tree": {tree}}
	return c.baseURL + "/job/" + url.PathEscape(name) + "/api/json?" + query.Encode()
}

func (c *jenkinsClient) buildPath(name string, number int64) string {
	query := url.Values{"tree": {"result,timestamp,url"}}
	return c.baseURL + "/job/" + url.PathEscape(name) + "/" +
		strconv.FormatInt(number, 10) + "/api/json?" + query.Encode()
}

func (c *jenkinsClient) computersPath() string {
	query := url.Values{"tree": {"computer[displayName,offline,offlineCauseReason]"}}
	return c.baseURL + "/computer/api/json?" + query.Encode()
}

// get decodes the JSON at path into out.  A 404 reply yields errNotFound.
func (c *jenkinsClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if c.user != "" && c.secret != "" {
		tok := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.secret))
		req.Header.Set("Authorization", "Basic "+tok)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode/100 != 2:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jenkins: http status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("jenkins: decode %s: %w", path, err)
	}
	return nil
}

// lastCompleted returns the latest completed build of the job, or nil
// if the job does not exist or has never completed a build.
func (c *jenkinsClient) lastCompleted(name string) (*jenkinsBuild, error) {
	var job jenkinsJob
	tree := "color,lastBuild[number,result],lastCompletedBuild[number,result,timestamp,url]"
	err := c.get(c.jobPath(name, tree), &job)
	if err != nil {
		return nil, err
	}
	return job.LastCompletedBuild, nil
}

// build fetches the result of one build; missing builds (404, e.g. pruned)
// yield errNotFound.
func (c *jenkinsClient) build(name string, number int64) (*jenkinsBuild, error) {
	var b jenkinsBuild
	if err := c.get(c.buildPath(name, number), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// computers lists the Jenkins nodes (computers).
func (c *jenkinsClient) computers() ([]jenkinsNode, error) {
	var set struct {
		Computer []jenkinsNode `json:"computer"`
	}
	if err := c.get(c.computersPath(), &set); err != nil {
		return nil, err
	}
	return set.Computer, nil
}
