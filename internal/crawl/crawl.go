package crawl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	v1 "floss.fund/portal/internal/schemas/v1"
)

type Schema interface {
	Validate(v1.Manifest) (v1.Manifest, error)
}

type Opt struct {
	UserAgent    string        `json:"useragent"`
	MaxHostConns int           `json:"max_host_conns"`
	ReqTimeout   time.Duration `json:"req_timeout"`
	Attempts     int           `json:"attempts"`
	MaxBytes     int64         `json:"max_bytes"`
}

type Crawl struct {
	opt     *Opt
	sc      Schema
	headers http.Header
	hc      *http.Client
	lo      *log.Logger
}

func New(o *Opt, sc Schema, lo *log.Logger) *Crawl {
	h := http.Header{}
	h.Set("User-Agent", o.UserAgent)

	return &Crawl{
		opt:     o,
		sc:      sc,
		headers: h,
		hc: &http.Client{
			Timeout: o.ReqTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   o.MaxHostConns,
				MaxConnsPerHost:       o.MaxHostConns,
				ResponseHeaderTimeout: o.ReqTimeout,
				IdleConnTimeout:       o.ReqTimeout,
			},
		},
		lo: lo,
	}
}

// FetchManifest fetches a given funding.json manifest, parses it, and returns.
func (c *Crawl) FetchManifest(manifestURL string) (v1.Manifest, error) {
	b, err := c.fetch(manifestURL)
	if err != nil {
		return v1.Manifest{}, err
	}

	return c.ParseManifest(b, manifestURL, true)
}

// ParseManifest parses a given JSON body, validates it, and returns the manifest.
func (c *Crawl) ParseManifest(b []byte, manifestURL string, checkProvenance bool) (v1.Manifest, error) {
	var m v1.Manifest
	if err := m.UnmarshalJSON(b); err != nil {
		return m, fmt.Errorf("error parsing JSON body: %v", err)
	}

	// Validate the manifest's schema.
	m.URL = manifestURL
	m.Body = json.RawMessage(b)
	if v, err := c.sc.Validate(m); err != nil {
		return v, err
	} else {
		m = v
	}

	// Establish the provenance of all URLs mentioned in the manifest.
	if checkProvenance {
		if err := c.CheckProvenance(m.Entity.WebpageURL, manifestURL); err != nil {
			return m, err
		}

		for _, o := range m.Projects {
			if err := c.CheckProvenance(o.WebpageURL, manifestURL); err != nil {
				return m, err
			}
			if err := c.CheckProvenance(o.RepositoryUrl, manifestURL); err != nil {
				return m, err
			}
		}
	}

	return m, nil
}

// CheckProvenance fetches the .well-known URL list for the given u and checks
// wehther the manifestURL is present in it, establishing its provenance.
func (c *Crawl) CheckProvenance(u v1.URL, manifestURL string) error {
	if u.WellKnown == "" {
		return nil
	}

	body, err := c.fetch(u.WellKnown)
	if err != nil {
		return err
	}

	ub := []byte(manifestURL)
	for n, b := range bytes.Split(body, []byte("\n")) {
		if bytes.Equal(ub, b) {
			return nil
		}

		if n > 100 {
			return errors.New("too many lines in the .well-known list")
		}
	}

	return fmt.Errorf("manifest URL %s was not found in the .well-known list", manifestURL)
}

// fetch fetches a given URL with error retries.
func (c *Crawl) fetch(u string) ([]byte, error) {
	var (
		body  []byte
		err   error
		retry bool
	)

	// Retry N times.
	for n := 0; n < c.opt.Attempts; n++ {
		body, retry, err = c.doReq(http.MethodGet, u, nil, c.headers)
		if err == nil || !retry {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	return body, nil
}

// doReq executes an HTTP doReq. The bool indicates whether it's a retriable error.
func (c *Crawl) doReq(method, rURL string, reqBody []byte, headers http.Header) (respBody []byte, retry bool, retErr error) {
	var (
		err      error
		postBody io.Reader
	)

	defer func() {
		msg := "OK"
		if retErr != nil {
			msg = retErr.Error()
		}
		c.lo.Printf("%s %s -> %v", method, rURL, msg)
	}()

	// Encode POST / PUT params.
	if method == http.MethodPost || method == http.MethodPut {
		postBody = bytes.NewReader(reqBody)
	}

	req, err := http.NewRequest(method, rURL, postBody)
	if err != nil {
		return nil, true, err
	}

	if headers != nil {
		req.Header = headers
	} else {
		req.Header = http.Header{}
	}

	// If a content-type isn't set, set the default one.
	if req.Header.Get("Content-Type") == "" {
		if method == http.MethodPost || method == http.MethodPut {
			req.Header.Add("Content-Type", "application/json")
		}
	}

	// If the request method is GET or DELETE, add the params as QueryString.
	if method == http.MethodGet || method == http.MethodDelete {
		req.URL.RawQuery = string(reqBody)
	}

	r, err := c.hc.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer func() {
		// Drain and close the body to let the Transport reuse the connection
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}()

	if r.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%s returned %d", rURL, r.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, c.opt.MaxBytes))
	if err != nil {
		return nil, true, err
	}

	return body, false, nil
}
