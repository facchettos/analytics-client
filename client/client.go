package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	klog "k8s.io/klog/v2"
)

const (
	defaultEndpoint = "http://analytics.loft.rocks/v1/insert"

	eventsCountThreshold = 100

	maxUploadInterval = 5 * time.Minute
	minUploadInterval = time.Second * 30
)

func NewClient() Client {
	c := &client{
		endpoint: defaultEndpoint,

		buffer:   newEventBuffer(eventsCountThreshold),
		overflow: newEventBuffer(eventsCountThreshold),

		events: make(chan Event, 100),
	}

	// start sending events in an interval
	go c.loop()

	return c
}

type client struct {
	buffer        *eventBuffer
	overflow      *eventBuffer
	droppedEvents int
	bufferMutex   sync.Mutex

	events chan Event

	endpoint string
}

func (c *client) RecordEvent(event Event) {
	c.events <- event
}

func (c *client) Flush() {
	c.executeUpload(c.exchangeBuffer())
}

func (c *client) loop() {
	// constantly pull events into this buffer
	go func() {
		for event := range c.events {
			// try to write into buffer first and fallback to overflow buffer
			c.bufferMutex.Lock()
			if !c.buffer.Append(event) && !c.overflow.Append(event) {
				c.droppedEvents++
			}
			c.bufferMutex.Unlock()
		}
	}()

	// constantly loop
	for {
		// either wait until buffer is full or up to 5 minutes
		startWait := time.Now()
		c.bufferMutex.Lock()
		fullChan := c.buffer.Full()
		c.bufferMutex.Unlock()

		// wait until buffer is full or time is up
		select {
		case <-fullChan:
			timeSinceStart := time.Since(startWait)
			if timeSinceStart < minUploadInterval {
				// wait the rest of the time here before proceeding
				time.Sleep(minUploadInterval - timeSinceStart)
			}
		case <-time.After(maxUploadInterval):
		}

		// flush the buffer
		c.Flush()
	}
}

func (c *client) executeUpload(buffer []Event) {
	if len(buffer) == 0 {
		return
	}

	// marshal request
	marshaled, err := json.Marshal(&Request{
		Data: buffer,
	})
	if err != nil {
		klog.V(1).ErrorS(err, "failed to json.Marshal analytics request")
		return
	}

	// send the telemetry data and ignore the response
	resp, err := http.Post(c.endpoint, "application/json", bytes.NewReader(marshaled))
	if err != nil {
		klog.V(1).ErrorS(err, "error sending analytics request")
	} else if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		klog.V(1).ErrorS(fmt.Errorf("%s%w", string(out), err), "analytics request returned non 200 status code")
	}
}

func (c *client) exchangeBuffer() []Event {
	c.bufferMutex.Lock()
	defer c.bufferMutex.Unlock()

	if c.droppedEvents > 0 {
		klog.V(1).InfoS("events were dropped because analytics buffer was full", "events", c.droppedEvents)
	}

	events := c.buffer.Drain()
	c.buffer = c.overflow
	c.overflow = newEventBuffer(eventsCountThreshold)
	c.droppedEvents = 0
	return events
}
