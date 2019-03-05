package loader

import (
	"bytes"
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

type LoadCfg struct {
	duration           int //seconds
	goroutines         int
	testUrl            string
	reqBody            string
	method             string
	host               string
	header             map[string]string
	statsAggregator    chan *RequesterStats
	timeoutms          int
	allowRedirects     bool
	disableCompression bool
	disableKeepAlive   bool
	interrupted        int32
	clientCert         string
	clientKey          string
	caCert             string
	http2              bool
	hold               chan struct{}
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

func NewLoadCfg(duration int, //seconds
	goroutines int,
	testUrl string,
	reqBody string,
	method string,
	host string,
	header map[string]string,
	statsAggregator chan *RequesterStats,
	timeoutms int,
	allowRedirects bool,
	disableCompression bool,
	disableKeepAlive bool,
	clientCert string,
	clientKey string,
	caCert string,
	http2 bool) (rt *LoadCfg) {
	rt = &LoadCfg{duration, goroutines, testUrl, reqBody, method, host, header, statsAggregator, timeoutms,
		allowRedirects, disableCompression, disableKeepAlive, 0, clientCert, clientKey, caCert, http2, make(chan struct{}) }
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
func (load *LoadCfg) DoRequest(httpClient *http.Client, header map[string]string, method, host, loadUrl, reqBody string) (respSize int, item *RequesterItem) {
	respSize = -1

	loadUrl = escapeUrlStr(loadUrl)

	var buf io.Reader
	if len(reqBody) > 0 {
		buf = bytes.NewBufferString(reqBody)
	}

	req, err := http.NewRequest(method, loadUrl, buf)
	if err != nil {
		fmt.Println("An error occured doing request", err)
		return
	}

	for hk, hv := range header {
		req.Header.Add(hk, hv)
	}

	// req.Header.Add("User-Agent", USER_AGENT)
	if host != "" {
		req.Host = host
	}
	start := time.Now()
	item = &RequesterItem{
		Time: start,
	}
	<-load.hold    // check hold

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
func (cfg *LoadCfg) RunSingleLoadSession(id string) {
	stats := &RequesterStats{
		MinRequestTime: time.Minute,
		Items: make([]*RequesterItem, 0, 10000),
	}
	start := time.Now()

	httpClient, err := client(cfg.disableCompression, cfg.disableKeepAlive, cfg.timeoutms, cfg.allowRedirects, cfg.clientCert, cfg.clientKey, cfg.caCert, cfg.http2)
	if err != nil {
		log.Fatal(err)
	}

	seq := 0
	for time.Since(start).Seconds() <= float64(cfg.duration) && atomic.LoadInt32(&cfg.interrupted) == 0 {
		respSize, item := cfg.DoRequest(httpClient, cfg.header, cfg.method, cfg.host, cfg.testUrl, cfg.reqBody)
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
	}
	cfg.statsAggregator <- stats
}

func (cfg *LoadCfg) Stop() {
	atomic.StoreInt32(&cfg.interrupted, 1)
}
