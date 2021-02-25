package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func main() {
	versions, _ := getVersionDeps()

	e := gin.Default()

	// bind website
	{
		e.LoadHTMLFiles("finder.html", "validator.html")

		e.GET("/validate", func(c *gin.Context) {
			c.HTML(http.StatusOK, "validator.html", ValidateResponse{
				GoVersion:      "1.15.8",
				KrakendVersion: "v1.3.0",
			})
		})

		e.POST("/validate/:krakend_version/:go_version", func(c *gin.Context) {
			version := c.Param("krakend_version")
			goVersion := c.Param("go_version")
			a, ok := versions[version]
			if !ok {
				c.AbortWithStatus(404)
				return
			}
			diffs := checkLines(a, goVersion, parseSumFile(c.Request.Body))

			c.HTML(http.StatusOK, "validator.html", ValidateResponse{
				GoVersion:      goVersion,
				KrakendVersion: version,
				Result:         diffs,
			})
		})

		e.GET("/versions/:version", func(c *gin.Context) {
			version := c.Param("version")
			if v, ok := versions[version]; ok {
				c.HTML(http.StatusOK, "finder.html", VersionResponse{Name: version, Version: v})
				return
			}
			c.AbortWithStatus(404)
		})

		e.GET("/", func(c *gin.Context) {
			c.HTML(http.StatusOK, "finder.html", VersionResponse{Name: "v1.3.0", Version: versions["v1.3.0"]})
		})
	}

	// bind api
	{
		e.GET("/api/versions/:version", func(c *gin.Context) {
			version := c.Param("version")
			if v, ok := versions[version]; ok {
				c.JSON(200, v)
				return
			}
			c.AbortWithStatus(404)
		})

		e.GET("/api/versions", func(c *gin.Context) {
			c.JSON(200, versions)
		})

		e.POST("/api/versions/:krakend_version/:go_version", func(c *gin.Context) {
			version := c.Param("krakend_version")
			goVersion := c.Param("go_version")
			a, ok := versions[version]
			if !ok {
				c.AbortWithStatus(404)
				return
			}
			diffs := checkLines(a, goVersion, parseSumFile(c.Request.Body))
			if len(diffs) == 0 {
				c.JSON(200, []string{})
				return
			}

			c.JSON(400, diffs)

		})
	}
	e.Run(":8080")
}

func checkLines(version Version, goVersion string, lines []string) []Diff {
	deps := map[string]string{}
	for _, dep := range lines {
		parts := strings.Split(dep, " ")
		if len(parts) < 2 {
			log.Println("dep ignored:", dep)
			continue
		}
		cleanedVersion := cleanVersion(parts[1])

		if deps[parts[0]] >= cleanedVersion {
			continue
		}
		deps[parts[0]] = cleanedVersion
	}
	b := Version{
		Go:   goVersion,
		Deps: deps,
	}

	return checkVersion(version, b)
}

func getVersionDeps() (map[string]Version, error) {
	b, err := ioutil.ReadFile("versions.json")
	if err != nil {
		return getUpdatedVersionsDeps(), nil
	}
	versions := map[string]Version{}
	if err := json.Unmarshal(b, &versions); err != nil {
		return getUpdatedVersionsDeps(), nil
	}
	return versions, nil
}

func getUpdatedVersionsDeps() map[string]Version {
	client := newHTTPClient()

	versions := map[string]Version{}
	for _, v := range getTags(client) {
		fmt.Println("checking version:", v)
		versions[v] = getUpdatedVersionDeps(client, v)
	}

	return versions
}

func getUpdatedVersionDeps(c httpClient, v string) Version {
	deps := map[string]string{}
	for _, dep := range readSumFileLines(c, v) {
		parts := strings.Split(dep, " ")
		cleanedVersion := cleanVersion(parts[1])

		if deps[parts[0]] >= cleanedVersion {
			continue
		}
		deps[parts[0]] = cleanedVersion
	}
	return Version{
		Go:   "",
		Deps: deps,
	}
}

type httpClient func(*http.Request) (*http.Response, error)

func newHTTPClient() httpClient {
	timer := time.NewTicker(3 * time.Minute)
	type response struct {
		resp *http.Response
		err  error
	}
	type request struct {
		req *http.Request
		out chan response
	}

	in := make(chan request)

	go func() {
		for {
			<-timer.C
			r := <-in
			resp, err := http.DefaultClient.Do(r.req)
			r.out <- response{resp, err}
		}
	}()

	return func(req *http.Request) (*http.Response, error) {
		out := make(chan response)
		in <- request{req: req, out: out}
		r := <-out
		return r.resp, r.err
	}
}

func cleanVersion(v string) string {
	if l := len(v); l > 7 && v[l-7:] == "/go.mod" {
		return v[:l-7]
	}
	return v
}

func getTags(client httpClient) []string {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/devopsfaith/krakend-ce/tags", nil)
	req.Header.Add("user-agent", userAgent)
	resp, err := client(req)
	if err != nil {
		fmt.Println(err.Error())
		return []string{}
	}

	tags := Tags{}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		fmt.Println(err.Error())
		return []string{}
	}

	defer resp.Body.Close()
	result := make([]string, len(tags))

	for i, t := range tags {
		result[i] = t.Name
	}

	return result
}

type Tags []struct {
	Name       string `json:"name"`
	ZipballURL string `json:"zipball_url"`
	TarballURL string `json:"tarball_url"`
	Commit     struct {
		Sha string `json:"sha"`
		URL string `json:"url"`
	} `json:"commit"`
	NodeID string `json:"node_id"`
}

type ValidateResponse struct {
	GoVersion      string
	KrakendVersion string
	Result         []Diff
}

type VersionResponse struct {
	Name    string
	Version Version
}

type Version struct {
	Go   string
	Deps map[string]string
}

type Diff struct {
	Name     string
	Expected string
	Have     string
}

func checkVersion(a, b Version) []Diff {
	diffs := []Diff{}
	if a.Go != b.Go {
		diffs = append(diffs, Diff{Name: "go", Expected: a.Go, Have: b.Go})
	}

	for k, expect := range a.Deps {
		have, ok := b.Deps[k]
		if !ok {
			continue
		}
		if have != expect {
			diffs = append(diffs, Diff{Name: k, Expected: expect, Have: have})
		}
	}

	return diffs
}

func readSumFileLines(client httpClient, version string) []string {
	lines := []string{}
	url := fmt.Sprintf("https://raw.githubusercontent.com/devopsfaith/krakend-ce/%s/go.sum", version)
	req, nil := http.NewRequest("GET", url, nil)
	req.Header.Add("user-agent", userAgent)
	resp, err := client(req)
	if err != nil {
		fmt.Println(err.Error())
		return lines
	}
	lines = parseSumFile(resp.Body)
	resp.Body.Close()
	return lines
}

func parseSumFile(r io.Reader) []string {
	lines := []string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		lines = append(lines, scanner.Text()) // Println will add back the final '\n'
	}
	if err := scanner.Err(); err != nil {
		fmt.Println("reading standard input:", err.Error())
	}
	return lines
}

const (
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_4) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/84.0.4147.125 Safari/537.36"
)
