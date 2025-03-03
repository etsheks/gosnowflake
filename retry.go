// Copyright (c) 2017-2022 Snowflake Computing Inc. All rights reserved.

package gosnowflake

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var random *rand.Rand

func init() {
	random = rand.New(rand.NewSource(time.Now().UnixNano()))
}

const (
	// requestGUIDKey is attached to every request against Snowflake
	requestGUIDKey string = "request_guid"
	// retryCountKey is attached to query-request from the second time
	retryCountKey string = "retryCount"
	// retryReasonKey contains last HTTP status or 0 if timeout
	retryReasonKey string = "retryReason"
	// clientStartTime contains a time when client started request (first request, not retries)
	clientStartTimeKey string = "clientStartTime"
	// requestIDKey is attached to all requests to Snowflake
	requestIDKey string = "requestId"
)

// This class takes in an url during construction and replaces the value of
// request_guid every time replace() is called. If the url does not contain
// request_guid, just return the original url
type requestGUIDReplacer interface {
	// replace the url with new ID
	replace() *url.URL
}

// Make requestGUIDReplacer given a url string
func newRequestGUIDReplace(urlPtr *url.URL) requestGUIDReplacer {
	values, err := url.ParseQuery(urlPtr.RawQuery)
	if err != nil {
		// nop if invalid query parameters
		return &transientReplace{urlPtr}
	}
	if len(values.Get(requestGUIDKey)) == 0 {
		// nop if no request_guid is included.
		return &transientReplace{urlPtr}
	}

	return &requestGUIDReplace{urlPtr, values}
}

// this replacer does nothing but replace the url
type transientReplace struct {
	urlPtr *url.URL
}

func (replacer *transientReplace) replace() *url.URL {
	return replacer.urlPtr
}

/*
requestGUIDReplacer is a one-shot object that is created out of the retry loop and
called with replace to change the retry_guid's value upon every retry
*/
type requestGUIDReplace struct {
	urlPtr    *url.URL
	urlValues url.Values
}

/*
*
This function would replace they value of the requestGUIDKey in a url with a newly
generated UUID
*/
func (replacer *requestGUIDReplace) replace() *url.URL {
	replacer.urlValues.Del(requestGUIDKey)
	replacer.urlValues.Add(requestGUIDKey, NewUUID().String())
	replacer.urlPtr.RawQuery = replacer.urlValues.Encode()
	return replacer.urlPtr
}

type retryCountUpdater interface {
	replaceOrAdd(retry int) *url.URL
}

type retryCountUpdate struct {
	urlPtr    *url.URL
	urlValues url.Values
}

// this replacer does nothing but replace the url
type transientRetryCountUpdater struct {
	urlPtr *url.URL
}

func (replaceOrAdder *transientRetryCountUpdater) replaceOrAdd(retry int) *url.URL {
	return replaceOrAdder.urlPtr
}

func (replacer *retryCountUpdate) replaceOrAdd(retry int) *url.URL {
	replacer.urlValues.Del(retryCountKey)
	replacer.urlValues.Add(retryCountKey, strconv.Itoa(retry))
	replacer.urlPtr.RawQuery = replacer.urlValues.Encode()
	return replacer.urlPtr
}

func newRetryCountUpdater(urlPtr *url.URL) retryCountUpdater {
	if !isQueryRequest(urlPtr) {
		// nop if not query-request
		return &transientRetryCountUpdater{urlPtr}
	}
	values, err := url.ParseQuery(urlPtr.RawQuery)
	if err != nil {
		// nop if the URL is not valid
		return &transientRetryCountUpdater{urlPtr}
	}
	return &retryCountUpdate{urlPtr, values}
}

type retryReasonUpdater interface {
	replaceOrAdd(reason int) *url.URL
}

type retryReasonUpdate struct {
	url *url.URL
}

func (retryReasonUpdater *retryReasonUpdate) replaceOrAdd(reason int) *url.URL {
	query := retryReasonUpdater.url.Query()
	query.Del(retryReasonKey)
	query.Add(retryReasonKey, strconv.Itoa(reason))
	retryReasonUpdater.url.RawQuery = query.Encode()
	return retryReasonUpdater.url
}

type transientRetryReasonUpdater struct {
	url *url.URL
}

func (retryReasonUpdater *transientRetryReasonUpdater) replaceOrAdd(_ int) *url.URL {
	return retryReasonUpdater.url
}

func newRetryReasonUpdater(url *url.URL, cfg *Config) retryReasonUpdater {
	// not a query request
	if !isQueryRequest(url) {
		return &transientRetryReasonUpdater{url}
	}
	// implicitly disabled retry reason
	if cfg != nil && cfg.IncludeRetryReason == ConfigBoolFalse {
		return &transientRetryReasonUpdater{url}
	}
	return &retryReasonUpdate{url}
}

func ensureClientStartTimeIsSet(url *url.URL, clientStartTime string) *url.URL {
	if !isQueryRequest(url) {
		// nop if not query-request
		return url
	}
	query := url.Query()
	if query.Has(clientStartTimeKey) {
		return url
	}
	query.Add(clientStartTimeKey, clientStartTime)
	url.RawQuery = query.Encode()
	return url
}

func isQueryRequest(url *url.URL) bool {
	return strings.HasPrefix(url.Path, queryRequestPath)
}

type waitAlgo struct {
	mutex *sync.Mutex   // required for random.Int63n
	base  time.Duration // base wait time
	cap   time.Duration // maximum wait time
}

func randSecondDuration(n time.Duration) time.Duration {
	return time.Duration(random.Int63n(int64(n/time.Second))) * time.Second
}

// decorrelated jitter backoff
func (w *waitAlgo) decorr(attempt int, sleep time.Duration) time.Duration {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	t := 3*sleep - w.base
	switch {
	case t > 0:
		return durationMin(w.cap, randSecondDuration(t)+w.base)
	case t < 0:
		return durationMin(w.cap, randSecondDuration(-t)+3*sleep)
	}
	return w.base
}

var defaultWaitAlgo = &waitAlgo{
	mutex: &sync.Mutex{},
	base:  5 * time.Second,
	cap:   160 * time.Second,
}

type requestFunc func(method, urlStr string, body io.Reader) (*http.Request, error)

type clientInterface interface {
	Do(req *http.Request) (*http.Response, error)
}

type retryHTTP struct {
	ctx                 context.Context
	client              clientInterface
	req                 requestFunc
	method              string
	fullURL             *url.URL
	headers             map[string]string
	bodyCreator         bodyCreatorType
	timeout             time.Duration
	raise4XX            bool
	currentTimeProvider currentTimeProvider
	cfg                 *Config
}

func newRetryHTTP(ctx context.Context,
	client clientInterface,
	req requestFunc,
	fullURL *url.URL,
	headers map[string]string,
	timeout time.Duration,
	currentTimeProvider currentTimeProvider,
	cfg *Config) *retryHTTP {
	instance := retryHTTP{}
	instance.ctx = ctx
	instance.client = client
	instance.req = req
	instance.method = "GET"
	instance.fullURL = fullURL
	instance.headers = headers
	instance.timeout = timeout
	instance.bodyCreator = emptyBodyCreator
	instance.raise4XX = false
	instance.currentTimeProvider = currentTimeProvider
	instance.cfg = cfg
	return &instance
}

func (r *retryHTTP) doRaise4XX(raise4XX bool) *retryHTTP {
	r.raise4XX = raise4XX
	return r
}

func (r *retryHTTP) doPost() *retryHTTP {
	r.method = "POST"
	return r
}

func (r *retryHTTP) setBody(body []byte) *retryHTTP {
	r.bodyCreator = func() ([]byte, error) {
		return body, nil
	}
	return r
}

func (r *retryHTTP) setBodyCreator(bodyCreator bodyCreatorType) *retryHTTP {
	r.bodyCreator = bodyCreator
	return r
}

func (r *retryHTTP) execute() (res *http.Response, err error) {
	totalTimeout := r.timeout
	logger.WithContext(r.ctx).Infof("retryHTTP.totalTimeout: %v", totalTimeout)
	retryCounter := 0
	sleepTime := time.Duration(0)
	clientStartTime := strconv.FormatInt(r.currentTimeProvider.currentTime(), 10)

	var requestGUIDReplacer requestGUIDReplacer
	var retryCountUpdater retryCountUpdater
	var retryReasonUpdater retryReasonUpdater

	for {
		logger.Debugf("retry count: %v", retryCounter)
		body, err := r.bodyCreator()
		if err != nil {
			return nil, err
		}
		req, err := r.req(r.method, r.fullURL.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if req != nil {
			// req can be nil in tests
			req = req.WithContext(r.ctx)
		}
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		res, err = r.client.Do(req)
		if err != nil {
			// check if it can retry.
			doExit, err := r.isRetryableError(err)
			if doExit {
				return res, err
			}
			// cannot just return 4xx and 5xx status as the error can be sporadic. run often helps.
			logger.WithContext(r.ctx).Warningf(
				"failed http connection. no response is returned. err: %v. retrying...\n", err)
		} else {
			if res.StatusCode == http.StatusOK || r.raise4XX && res != nil && res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 429 {
				// exit if success
				// or
				// abort connection if raise4XX flag is enabled and the range of HTTP status code are 4XX.
				// This is currently used for Snowflake login. The caller must generate an error object based on HTTP status.
				break
			}
			logger.WithContext(r.ctx).Warningf(
				"failed http connection. HTTP Status: %v. retrying...\n", res.StatusCode)
			res.Body.Close()
		}
		// uses decorrelated jitter backoff
		sleepTime = defaultWaitAlgo.decorr(retryCounter, sleepTime)

		if totalTimeout > 0 {
			logger.WithContext(r.ctx).Infof("to timeout: %v", totalTimeout)
			// if any timeout is set
			totalTimeout -= sleepTime
			if totalTimeout <= 0 {
				if err != nil {
					return nil, err
				}
				if res != nil {
					return nil, fmt.Errorf("timeout after %s. HTTP Status: %v. Hanging?", r.timeout, res.StatusCode)
				}
				return nil, fmt.Errorf("timeout after %s. Hanging?", r.timeout)
			}
		}
		retryCounter++
		if requestGUIDReplacer == nil {
			requestGUIDReplacer = newRequestGUIDReplace(r.fullURL)
		}
		r.fullURL = requestGUIDReplacer.replace()
		if retryCountUpdater == nil {
			retryCountUpdater = newRetryCountUpdater(r.fullURL)
		}
		r.fullURL = retryCountUpdater.replaceOrAdd(retryCounter)
		if retryReasonUpdater == nil {
			retryReasonUpdater = newRetryReasonUpdater(r.fullURL, r.cfg)
		}
		retryReason := 0
		if res != nil {
			retryReason = res.StatusCode
		}
		r.fullURL = retryReasonUpdater.replaceOrAdd(retryReason)
		r.fullURL = ensureClientStartTimeIsSet(r.fullURL, clientStartTime)
		logger.WithContext(r.ctx).Infof("sleeping %v. to timeout: %v. retrying", sleepTime, totalTimeout)
		logger.WithContext(r.ctx).Infof("retry count: %v, retry reason: %v", retryCounter, retryReason)

		await := time.NewTimer(sleepTime)
		select {
		case <-await.C:
			// retry the request
		case <-r.ctx.Done():
			await.Stop()
			return res, r.ctx.Err()
		}
	}
	return res, err
}

func (r *retryHTTP) isRetryableError(err error) (bool, error) {
	urlError, isURLError := err.(*url.Error)
	if isURLError {
		// context cancel or timeout
		if urlError.Err == context.DeadlineExceeded || urlError.Err == context.Canceled {
			return true, urlError.Err
		}
		if driverError, ok := urlError.Err.(*SnowflakeError); ok {
			// Certificate Revoked
			if driverError.Number == ErrOCSPStatusRevoked {
				return true, err
			}
		}
		if _, ok := urlError.Err.(x509.CertificateInvalidError); ok {
			// Certificate is invalid
			return true, err
		}
		if _, ok := urlError.Err.(x509.UnknownAuthorityError); ok {
			// Certificate is self-signed
			return true, err
		}
		errString := urlError.Err.Error()
		if runtime.GOOS == "darwin" && strings.HasPrefix(errString, "x509:") && strings.HasSuffix(errString, "certificate is expired") {
			// Certificate is expired
			return true, err
		}

	}
	return false, err
}
