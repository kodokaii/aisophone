package main

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
)

type AudioPipe struct {
	buf    bytes.Buffer
	cond   *sync.Cond
	closed atomic.Bool
}

func NewAudioPipe() *AudioPipe {
	ap := &AudioPipe{
		cond: sync.NewCond(new(sync.Mutex)),
	}
	return ap
}

func (ap *AudioPipe) Len() int {
	return ap.buf.Len()
}

func (ap *AudioPipe) Write(audio []byte) (int, error) {
	if ap.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	ap.cond.L.Lock()
	defer ap.cond.L.Unlock()

	n, _ := ap.buf.Write(audio)
	ap.cond.Signal()
	return n, nil
}

func (ap *AudioPipe) Read(audio []byte) (int, error) {
	ap.cond.L.Lock()
	defer ap.cond.L.Unlock()

	for ap.buf.Len() == 0 {
		if ap.closed.Load() {
			return 0, io.EOF
		}
		ap.cond.Wait()
	}
	n, err := ap.buf.Read(audio)
	return n, err
}

func (ap *AudioPipe) Clear() {
	ap.cond.L.Lock()
	defer ap.cond.L.Unlock()

	ap.buf.Reset()
}

func (ap *AudioPipe) Close() {
	ap.closed.Store(true)
	ap.cond.Broadcast()
}
