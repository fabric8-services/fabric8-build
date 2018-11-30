package rest

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
)

// AbsoluteURL prefixes a relative URL with absolute address
func AbsoluteURL(req *http.Request, relative string) string {
	scheme := "http"
	if req.URL != nil && req.URL.Scheme == "https" { // isHTTPS
		scheme = "https"
	}
	xForwardProto := req.Header.Get("X-Forwarded-Proto")
	if xForwardProto != "" {
		scheme = xForwardProto
	}
	return fmt.Sprintf("%s://%s%s", scheme, req.Host, relative)
}

// AbsoluteURLAsURL returns the result of AbsoluteURL parsed into a URL
// structure and a potential parsing error.
func AbsoluteURLAsURL(req *http.Request, relative string) (*url.URL, error) {
	return url.Parse(AbsoluteURL(req, relative))
}

// ReadBody reads body from a ReadCloser and returns it as a string
func ReadBody(body io.ReadCloser) string {
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(body)
	return buf.String()
}

// CloseResponse reads the body and close the response. To be used to prevent file descriptor leaks.
func CloseResponse(response *http.Response) {
	_, _ = ioutil.ReadAll(response.Body)
	response.Body.Close()
}
