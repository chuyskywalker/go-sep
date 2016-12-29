package main

import (
	"bytes"
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/nu7hatch/gouuid"
	"io"
	"io/ioutil"
	"net/http"
	"runtime"
	"time"
)

// targetList is a map of queuenames to targets for mapping events through to other systems
type targetList map[string][]eventTarget

// eventTarget holds the structure for targets under a targetList
type eventTarget struct {
	// URL is the endpoint which an event will be repeated at
	URL string
	// BufferLen is how large the chan will be made for this EventTarget
	BufferLen uint64
}

// requestMessage represents an http event to be repeated to eventTargets
// This is, in essence, an http.Request, but those aren't very clean to pass around
type requestMessage struct {
	UUID    string
	URL     string
	Method  string
	Source  string
	Headers http.Header
	Body    []byte
}

func defaultHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("No such event endpoint"))
}

func handleIncomingEvent(w http.ResponseWriter, r *http.Request) {

	// get a UUID for this transaction
	u5, err := uuid.NewV4()
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	body, _ := ioutil.ReadAll(r.Body)

	requestObj := requestMessage{
		UUID:    u5.String(),
		URL:     r.RequestURI,
		Method:  r.Method,
		Source:  r.RemoteAddr,
		Headers: r.Header,
		Body:    body,
	}

	queue := requestObj.URL[1:]

	fmt.Fprintf(w, "{ \"id\":\"%s\" }\n", u5) // to lazy to do a real json.Marshal, etc

	for _, eventTarget := range targets[queue] {
		qu := queueURL{queue, eventTarget.URL}
		addchan <- qu
		// this select/case/default is a non-blocking chan push
		select {
		case sendPool[qu].RequestChan <- requestObj:
		default:
			// metricize that we're dropping messages
			dellchan <- qu
			// kill off the oldest, not-in-flight message
			// todo: it could possibly make sense to kill the inflight message, but...have to think on that more
			<-sendPool[qu].RequestChan
			// we attempt to send the current message one last time, but this it not guaranteed to work
			select {
			case sendPool[qu].RequestChan <- requestObj:
			default:
				// well, we tried our damndest, log it and move on
				log.WithFields(log.Fields{
					"id":    u5,
					"queue": queue,
					"url":   eventTarget.URL,
				}).Info("queue-full-message-lost")
			}
		}
	}
}

func sendEvent(client *http.Client, qu queueURL, req requestMessage) {
	start := time.Now()
	var sent bool
	sent = false
	attempts := 0
	sleepDuration := time.Millisecond * 100
	for {
		attempts++
		httpReq, _ := http.NewRequest(req.Method, qu.URL, bytes.NewBuffer(req.Body))
		for headerName, values := range req.Headers {
			for _, value := range values {
				httpReq.Header.Add(headerName, value)
			}
		}
		httpReq.Header.Set("X-Wsq-Id", req.UUID)
		resp, err := client.Do(httpReq)

		if err == nil {
			// get rid of the response, we don't care
			// but we do need to clean it out, so the client can reuse the same connection
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
		}

		if err == nil && resp.StatusCode == 200 {
			sent = true
			break
		}

		// max duration, ever
		// todo: make this configurable
		if time.Since(start) > time.Second*60 {
			break
		}

		// oops, didn't work; have a pause and try again in a bit
		time.Sleep(sleepDuration)

		// slowly ramp up our sleep interval, shall we? But cap it too
		if sleepDuration < time.Duration(time.Second*15) {
			sleepDuration = time.Duration(float64(sleepDuration) * 1.5)
		} else {
			sleepDuration = time.Duration(time.Second * 15)
		}
	}
	elapsed := time.Since(start)

	if sent {
		deltchan <- qu
	} else {
		delfchan <- qu
	}

	log.WithFields(log.Fields{
		"id":       req.UUID,
		"queue":    qu.Queue,
		"url":      qu.URL,
		"attempts": attempts,
		"sent":     sent,
		"duration": elapsed.Seconds() * 1e3, /* ms hack */
	}).Info("relay-end")
}

// queueUrl is a structure for uniqu'ifying tracking maps
type queueURL struct {
	Queue string
	URL   string
}

type counterVals struct {
	Current uint64
	Total   uint64
	Success uint64
	Failure uint64
	Lost    uint64
}

var counters = make(map[queueURL]counterVals)
var addchan = make(chan queueURL, 100)
var deltchan = make(chan queueURL, 100)
var delfchan = make(chan queueURL, 100)
var dellchan = make(chan queueURL, 100)

type worker struct {
	QueueURL    queueURL
	RequestChan chan requestMessage
	QuitChan    chan bool
}

func (w worker) Start() {
	go func() {
		client := &http.Client{
			// todo: reasonable default?
			Timeout: 10 * time.Second,
			// no cookies, please
			Jar: nil,
		}
		for {
			work := <-w.RequestChan
			sendEvent(client, w.QueueURL, work)
		}
	}()
}

var sendPool = make(map[queueURL]worker)

var targets targetList

func main() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.RFC3339Nano,
	})

	configID := flag.String("config", "default", "Which stanza of the config to use")
	flag.Parse()

	var ok bool
	targets, ok = allTargets[*configID]
	if !ok {
		panic("Could not load expected configuration")
	}

	// initialize counters to zero
	// You don't _have_ to do this, but I like having all the counters
	// reporting 0 immediately for stat collection purposes.
	for queue, eventTargets := range targets {
		for _, eventTarget := range eventTargets {
			qu := queueURL{queue, eventTarget.URL}
			counters[qu] = counterVals{0, 0, 0, 0, 0}
			if eventTarget.BufferLen <= 0 {
				panic("Buffer length must be > 0")
			}
			sendPool[qu] = worker{
				QueueURL:    qu,
				RequestChan: make(chan requestMessage, eventTarget.BufferLen),
				QuitChan:    make(chan bool),
			}
			sendPool[qu].Start()
		}
	}

	// goroutine to keep the counters up-to-date
	go func() {
		for {
			// watch each channel as items rolls in and modify the counters as needed
			select {
			// you can't do counters[control].Current++ in go, so this mess is what results
			case control := <-addchan:
				tmp := counters[control]
				tmp.Current++
				tmp.Total++
				counters[control] = tmp
			case control := <-deltchan:
				tmp := counters[control]
				tmp.Current--
				tmp.Success++
				counters[control] = tmp
			case control := <-delfchan:
				tmp := counters[control]
				tmp.Current--
				tmp.Failure++
				counters[control] = tmp
			case control := <-dellchan:
				tmp := counters[control]
				tmp.Current--
				tmp.Lost++
				counters[control] = tmp
			}
		}
	}()

	// A dumb goroutine to watch memory usage and counter metrics
	go func() {
		var mem runtime.MemStats
		for {
			runtime.ReadMemStats(&mem)
			log.WithFields(log.Fields{
				"mem.Alloc":            mem.Alloc,
				"mem.TotalAlloc":       mem.TotalAlloc,
				"mem.HeapAlloc":        mem.HeapAlloc,
				"mem.HeapSys":          mem.HeapSys,
				"runtime.NumGoroutine": runtime.NumGoroutine(),
			}).Info("metrics-mem")
			for cKeys, cVals := range counters {
				log.WithFields(log.Fields{
					"queue":   cKeys.Queue,
					"url":     cKeys.URL,
					"current": cVals.Current,
					"total":   cVals.Total,
					"success": cVals.Success,
					"failure": cVals.Failure,
					"lost":    cVals.Lost,
					"chanlen": len(sendPool[cKeys].RequestChan),
					"chanmax": cap(sendPool[cKeys].RequestChan),
				}).Info("metrics-queue")
			}
			time.Sleep(time.Second * 5)
		}
	}()

	// Oh, hey, there's the webserver!
	log.Info("starting server")
	for queue := range targets {
		log.Info("registering queue @ /" + queue)
		http.HandleFunc("/"+queue, handleIncomingEvent)
	}
	http.HandleFunc("/", defaultHandler)
	err := http.ListenAndServe(":8000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}

}
