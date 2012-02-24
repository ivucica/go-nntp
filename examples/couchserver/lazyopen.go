package main

import (
	"errors"
	"io"
	"io/ioutil"
	"net/http"
)

type lazyOpener struct {
	url  string
	data []byte
	err  error
}

func (l *lazyOpener) init() {
	res, err := http.Get(l.url)
	l.err = err
	if err == nil {
		if res.StatusCode != 200 {
			l.err = errors.New(res.Status)
		}
		defer res.Body.Close()
		l.data, l.err = ioutil.ReadAll(res.Body)
	}
}

func (l *lazyOpener) Read(p []byte) (n int, err error) {
	if l.data == nil && l.err == nil {
		l.init()
	}
	if l.err != nil {
		return 0, err
	}
	if len(l.data) == 0 {
		return 0, io.EOF
	}
	copied := copy(p, l.data)
	l.data = l.data[copied:]
	return copied, nil
}
