package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"github.com/patrickmn/go-cache"
	"runtime"
	"time"
	"strings"
)

// Console flags
var (
	listen            = flag.String("l", ":8888", "port to accept requests")
	targetProduction  = flag.String("a", "localhost:8080", "where production traffic goes. http://localhost:8080/production")
	altTarget         = flag.String("b", "localhost:8081", "where testing traffic goes. response are skipped. http://localhost:8081/test")
	debug             = flag.Bool("debug", false, "more logging, showing ignored output")
	productionTimeout = flag.Int("a.timeout", 3, "timeout in seconds for production traffic")
	alternateTimeout  = flag.Int("b.timeout", 1, "timeout in seconds for alternate site traffic")
)

// handler contains the address of the main Target and the one for the Alternative target
type handler struct {
	Target      string
	Alternative string
	SessionCache *cache.Cache
}

// ServeHTTP duplicates the incoming request (req) and does the request to the Target and the Alternate target discading the Alternate response
func (h handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	alternativeRequest, productionRequest := DuplicateRequest(req)

	cookieName := "PHPSESSID"
	cookie, err := req.Cookie(cookieName)
	if err != nil {
		fmt.Printf("Failed to read cookie from request %s: %v\n", cookieName, err)
	}
	if cookie != nil {
		alternativeSessionId, found := h.SessionCache.Get(cookie.Value)
		if found {
			fmt.Println("lookup HIT", cookie.Value, alternativeSessionId)
	  	alternateCookie := &http.Cookie{
			  Name:     cookie.Name,
			  Value:    fmt.Sprintf("%s", alternativeSessionId),
			  Path:     cookie.Path,
			  Domain:   cookie.Domain,
			  Expires:  cookie.Expires,
			  MaxAge:   cookie.MaxAge,
			  Secure:   cookie.Secure,
			  HttpOnly: cookie.HttpOnly,
			}
			alternativeRequest.Header.Del("Cookie")
	    alternativeRequest.AddCookie(alternateCookie)
	  } else {
			fmt.Println("lookup MISS", cookie.Value)
		}
	}

	// Open new TCP connection to the server
	clientTcpConn, err := net.DialTimeout("tcp", h.Target, time.Duration(time.Duration(*productionTimeout)*time.Second))
	if err != nil {
		fmt.Printf("Failed to connect to %s\n", h.Target)
		return
	}
	clientHttpConn := httputil.NewClientConn(clientTcpConn, nil) // Start a new HTTP connection on it
	defer clientHttpConn.Close()                                 // Close the connection to the server
	err = clientHttpConn.Write(productionRequest)                // Pass on the request
	if err != nil {
		fmt.Printf("Failed to send to %s: %v\n", h.Target, err)
		return
	}
	resp, err := clientHttpConn.Read(productionRequest) // Read back the reply
	if err != nil {
		fmt.Printf("Failed to receive from %s: %v\n", h.Target, err)
		return
	}

	productionCookie := FindCookie(resp, cookieName)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	body, _ := ioutil.ReadAll(resp.Body)
	w.Write(body)

	go func() {
		defer func() {
			if r := recover(); r != nil && *debug {
				fmt.Println("Recovered in f", r)
			}
		}()
		// Open new TCP connection to the server
		clientTcpConn, err := net.DialTimeout("tcp", h.Alternative, time.Duration(time.Duration(*alternateTimeout)*time.Second))
		if err != nil {
			if *debug {
				fmt.Printf("Failed to connect to %s\n", h.Alternative)
			}
			return
		}
		clientHttpConn := httputil.NewClientConn(clientTcpConn, nil) // Start a new HTTP connection on it
		defer clientHttpConn.Close()                                 // Close the connection to the server
		err = clientHttpConn.Write(alternativeRequest)                             // Pass on the request
		if err != nil {
			if *debug {
				fmt.Printf("Failed to send to %s: %v\n", h.Alternative, err)
			}
			return
		}
		alternativeResponse, err := clientHttpConn.Read(alternativeRequest) // Read back the reply
		if err != nil {
			if *debug {
				fmt.Printf("Failed to receive from %s: %v\n", h.Alternative, err)
			}
			return
		}

		if productionCookie != nil {
			alternativeCookie := FindCookie(alternativeResponse, cookieName)
			if alternativeCookie != nil {
				h.SessionCache.Set(productionCookie.Value, alternativeCookie.Value, cache.DefaultExpiration)
			}
		}
	}()
	defer func() {
		if r := recover(); r != nil && *debug {
			fmt.Println("Recovered in f", r)
		}
	}()
}

func main() {
	flag.Parse()
	runtime.GOMAXPROCS(runtime.NumCPU())

	local, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Printf("Failed to listen to %s\n", *listen)
		return
	}
	h := handler{
		Target:      *targetProduction,
		Alternative: *altTarget,
		SessionCache: cache.New(24*time.Hour, 60*time.Minute),  // 24h expiry, run every hour
	}
	http.Serve(local, h)
}

type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() error { return nil }

func FindCookie(resp *http.Response, cookieName string) (*http.Cookie) {
		for _, c := range resp.Cookies() {
			if strings.EqualFold(c.Name, cookieName) {
				return c
			}
		}
		return nil
}

func DuplicateRequest(request *http.Request) (request1 *http.Request, request2 *http.Request) {
	b1 := new(bytes.Buffer)
	b2 := new(bytes.Buffer)
	w := io.MultiWriter(b1, b2)
	io.Copy(w, request.Body)
	defer request.Body.Close()

	// create separate headers because we want to modify them later
	header1 := http.Header{}
	header2 := http.Header{}
	for k, v := range request.Header {
		values1 := make([]string, len(v))
		copy(values1, v)
		header1[k] = values1
		values2 := make([]string, len(v))
		copy(values2, v)
		header2[k] = values2
	}

	request1 = &http.Request{
		Method:        request.Method,
		URL:           request.URL,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header1,
		Body:          nopCloser{b1},
		Host:          request.Host,
		ContentLength: request.ContentLength,
	}
	request2 = &http.Request{
		Method:        request.Method,
		URL:           request.URL,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header2,
		Body:          nopCloser{b2},
		Host:          request.Host,
		ContentLength: request.ContentLength,
	}
	return
}
