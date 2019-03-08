package loader

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zhangjyr/go-wrk/util"
)

const (
	USER_AGENT = "go-wrk"
)

var (
	ERR_THROTTLED_BELOW_ZERO = errors.New("Throttled below zero.")
	ERR_THROTTLED_ABOVE_LIMIT = errors.New("Throttled above limit.")
)

type RequestCfg struct {
	TestUrl     string
	ReqBody     string
	Method      string
	Host        string
	RawHeader   string
	Header      map[string]string
	Hash        [sha256.Size]byte
}

func (req *RequestCfg) String() string {
	return fmt.Sprintf("%s %s\r\nHOST: %s\r\n%s\r\n%s", req.Method, req.TestUrl, req.Host, req.RawHeader, req.ReqBody)
}

func (req *RequestCfg) Sign() {
	req.Header = make(map[string]string)
	if req.RawHeader != "" {
		headerPairs := strings.Split(req.RawHeader, ";")
		for _, hdr := range headerPairs {
			hp := strings.Split(hdr, ":")
			req.Header[hp[0]] = hp[1]
		}
	}
	req.Hash = sha256.Sum256([]byte(req.String()))
}

func (req RequestCfg) Copy() *RequestCfg {
	req.Header = nil
	req.Hash = [sha256.Size]byte{}
	return &req
}

type RequesterItem struct {
	Time           time.Time
	StatusCode     int
	ResponseTime   time.Duration
}

func (r *RequesterItem) String() string {
	return fmt.Sprintf("%f,%d,%f", float64(r.Time.Unix()) + float64(r.Time.Nanosecond()) / 1e9,
		r.StatusCode, r.ResponseTime.Seconds())
}

// RequesterStats used for colelcting aggregate statistics
type RequesterStats struct {
	TotRespSize    int64
	TotDuration    time.Duration
	MinRequestTime time.Duration
	MaxRequestTime time.Duration
	NumRequests    int
	NumErrs        int
	Items          []*RequesterItem
}

type LoadCfg struct {
	duration           int //seconds
	goroutines         int
	request            *RequestCfg
	statsAggregator    chan *RequesterStats
	timeoutms          int
	allowRedirects     bool
	disableCompression bool
	disableKeepAlive   bool
	interrupted        int32
	threshold          int32
	clientCert         string
	clientKey          string
	caCert             string
	http2              bool
	hold               chan struct{}
}

func NewLoadCfg(duration int, //seconds
	goroutines int,
	testUrl string,
	reqBody string,
	method string,
	host string,
	header string,
	statsAggregator chan *RequesterStats,
	timeoutms int,
	allowRedirects bool,
	disableCompression bool,
	disableKeepAlive bool,
	clientCert string,
	clientKey string,
	caCert string,
	http2 bool) (rt *LoadCfg) {
	request := &RequestCfg {
		TestUrl: testUrl,
		ReqBody: reqBody,
		Method: method,
		Host: host,
		RawHeader: header,
	}
	rt = &LoadCfg{ duration, goroutines, request, statsAggregator, timeoutms, allowRedirects, disableCompression,
		disableKeepAlive, 0, int32(goroutines), clientCert, clientKey, caCert, http2, make(chan struct{}) }
	rt.request.Sign()
	close(rt.hold)
	return
}

func (load *LoadCfg) Hold() {
	select {
	case <- load.hold:
		// If stopped, start
		load.hold = make(chan struct{})
	default:
		// Pass to avoid hold twice
	}
}

func (load *LoadCfg) IsHold() bool {
	select {
	case <- load.hold:
		return false
	default:
		return true
	}
}

func (load *LoadCfg) Resume() {
	select {
	case <- load.hold:
		// Closed? Do nothing
	default:
		close(load.hold)
	}
}

func escapeUrlStr(in string) string {
	qm := strings.Index(in, "?")
	if qm != -1 {
		qry := in[qm+1:]
		qrys := strings.Split(qry, "&")
		var query string = ""
		var qEscaped string = ""
		var first bool = true
		for _, q := range qrys {
			qSplit := strings.Split(q, "=")
			if len(qSplit) == 2 {
				qEscaped = qSplit[0] + "=" + url.QueryEscape(qSplit[1])
			} else {
				qEscaped = qSplit[0]
			}
			if first {
				first = false
			} else {
				query += "&"
			}
			query += qEscaped

		}
		return in[:qm] + "?" + query
	} else {
		return in
	}
}

//DoRequest single request implementation. Returns the size of the response and its duration
//On error - returns -1 on both
func (load *LoadCfg) DoRequest(httpClient *http.Client, reqCfg *RequestCfg) (respSize int, item *RequesterItem) {
	respSize = -1

	loadUrl := escapeUrlStr(reqCfg.TestUrl)

	var buf io.Reader
	if len(reqCfg.ReqBody) > 0 {
		buf = bytes.NewBufferString(reqCfg.ReqBody)
	}

	req, err := http.NewRequest(reqCfg.Method, loadUrl, buf)
	if err != nil {
		fmt.Println("An error occured doing request", err)
		return
	}

	for hk, hv := range reqCfg.Header {
		req.Header.Add(hk, hv)
	}

	// req.Header.Add("User-Agent", USER_AGENT)
	if reqCfg.Host != "" {
		req.Host = reqCfg.Host
	}
	start := time.Now()
	item = &RequesterItem{
		Time: start,
	}
	if load.request.Hash == reqCfg.Hash {
		<-load.hold    // check hold
	}

	resp, err := httpClient.Do(req)
	item.ResponseTime = time.Since(start)
	item.StatusCode = 500
	if err != nil {
		fmt.Println("redirect?")
		//this is a bit weird. When redirection is prevented, a url.Error is retuned. This creates an issue to distinguish
		//between an invalid URL that was provided and and redirection error.
		rr, ok := err.(*url.Error)
		if !ok {
			fmt.Println("An error occured doing request", err, rr)
			return
		}
		fmt.Println("An error occured doing request", err)
	}

	if resp == nil {
		fmt.Println("empty response")
		return
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()
	item.StatusCode = resp.StatusCode

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("An error occured reading body", err)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		// duration = time.Since(start)
		respSize = len(body) + int(util.EstimateHttpHeadersSize(resp.Header))
	} else if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusTemporaryRedirect {
		// duration = time.Since(start)
		respSize = int(resp.ContentLength) + int(util.EstimateHttpHeadersSize(resp.Header))
	} else {
		fmt.Println("received status code", resp.StatusCode, "from", resp.Header, "content", string(body), req)
	}

	return
}

//Requester a go function for repeatedly making requests and aggregating statistics as long as required
//When it is done, it sends the results using the statsAggregator channel
func (load *LoadCfg) RunSingleLoadSession(mark int, id string) {
	stats := &RequesterStats{
		MinRequestTime: time.Minute,
		Items: make([]*RequesterItem, 0, 10000),
	}
	start := time.Now()

	httpClient, err := client(load.disableCompression, load.disableKeepAlive, load.timeoutms, load.allowRedirects, load.clientCert, load.clientKey, load.caCert, load.http2)
	if err != nil {
		log.Fatal(err)
	}

	seq := 0
	for time.Since(start).Seconds() <= float64(load.duration) && atomic.LoadInt32(&load.interrupted) == 0 {
		if mark < int(atomic.LoadInt32(&load.threshold)) {
			respSize, item := load.DoRequest(httpClient, load.request)
			if respSize > 0 {
				stats.TotRespSize += int64(respSize)
				stats.TotDuration += item.ResponseTime
				stats.MaxRequestTime = util.MaxDuration(item.ResponseTime, stats.MaxRequestTime)
				stats.MinRequestTime = util.MinDuration(item.ResponseTime, stats.MinRequestTime)
				stats.NumRequests++
			} else {
				stats.NumErrs++
			}
			if item != nil {
				stats.Items = append(stats.Items, item)
				// if item.ResponseTime > 1 * time.Second {
				// 	log.Printf("Detected long request: %s_%d", id, seq)
				// }
			}
			seq++
		} else {
			// Sleep 10ms to prevent excessive for loop
			time.Sleep(10 * time.Millisecond)
		}
	}
	load.statsAggregator <- stats
}

func (load *LoadCfg) Swap(testUrl string) string {
	return load.SwapRequest(&testUrl, nil, nil, nil, nil).TestUrl
}

func (load *LoadCfg) SwapHeader(header string) map[string]string {
	return load.SwapRequest(nil, nil, nil, nil, &header).Header
}

func (load *LoadCfg) SwapBody(reqBody string) string {
	return load.SwapRequest(nil, &reqBody, nil, nil, nil).ReqBody
}

func (load *LoadCfg) SwapRequest(testUrl *string, reqBody *string, method *string, host *string, header *string) *RequestCfg {
	old := load.request
	new := old.Copy()
	if testUrl != nil {
		new.TestUrl = *testUrl
	}
	if reqBody != nil {
		new.ReqBody = *reqBody
	}
	if method != nil {
		new.Method = *method
	}
	if host != nil {
		new.Host = *host
	}
	if header != nil {
		new.RawHeader = *header
	}
	new.Sign()
	load.request = new
	return old
}


func (load *LoadCfg) Throttle(num int) {
	atomic.StoreInt32(&load.threshold, int32(num))
}

func (load *LoadCfg) ThrottleUp(num int) error {
	new := atomic.AddInt32(&load.threshold, int32(num))
	if new > int32(load.goroutines) {
		atomic.CompareAndSwapInt32(&load.threshold, new, int32(load.goroutines))
		return ERR_THROTTLED_ABOVE_LIMIT
	} else {
		return nil
	}
}

func (load *LoadCfg) ThrottleDown(num int) error {
	new := atomic.AddInt32(&load.threshold, int32(-num))
	if new < 0 {
		atomic.CompareAndSwapInt32(&load.threshold, new, 0)
		return ERR_THROTTLED_BELOW_ZERO
	} else {
		return nil
	}
}

func (load *LoadCfg) Unthrottle() {
	atomic.StoreInt32(&load.threshold, int32(load.goroutines))
}

func (load *LoadCfg) Threshhold() int32 {
	return atomic.LoadInt32(&load.threshold)
}

func (load *LoadCfg) Stop() {
	atomic.StoreInt32(&load.interrupted, 1)
}
