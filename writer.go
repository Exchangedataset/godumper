package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"sync"
	"time"

	"github.com/exchangedataset/streamcommons"
	"github.com/exchangedataset/streamcommons/simulator"
)

// WriterBufferSize is the size of dataset buffer, 5MB
const WriterBufferSize = 5 * 1024 * 1024

// Writer writes messages to gzipped file
type Writer struct {
	closed        bool
	lastTimestamp int64
	lock          sync.Mutex
	fileTimestamp int64
	buffer        *bytes.Buffer
	writer        *bufio.Writer
	gwriter       *gzip.Writer
	exchange      string
	url           string
	directory     string
	alwaysDisk    bool
	logger        *log.Logger
	sim           simulator.Simulator
}

func (w *Writer) beforeWrite(timestamp int64) (correctedTimestamp int64, err error) {
	if w.closed {
		err = errors.New("tried to write to an already closed writer")
		return
	}

	// correct time from going backwards
	if timestamp < w.lastTimestamp {
		// time is running backwards
		// probably because of system time correction
		w.logger.Println("timestamp is older than the last observed, substituting it to the last observed value")
		timestamp = w.lastTimestamp
	}
	correctedTimestamp = timestamp

	// it creates new file for every minute
	minute := int64(time.Duration(timestamp) / time.Minute)
	lastMinute := int64(time.Duration(w.lastTimestamp) / time.Minute)

	// set timestamp as last write time, this have to be after lastMinute is calculated
	w.lastTimestamp = timestamp

	if minute == lastMinute {
		// continues to use the same stream & file name
		return
	}
	// time to split dataset

	isFirstFile := w.buffer == nil
	if isFirstFile {
		// create new buffer
		bufArr := make([]byte, 0, WriterBufferSize)
		w.buffer = bytes.NewBuffer(bufArr)
		// prepare buffer writer
		w.writer = bufio.NewWriter(w.buffer)
		// prepare gzip writer
		w.gwriter, err = gzip.NewWriterLevel(w.writer, gzip.BestCompression)
		if err != nil {
			return
		}

		// write start line
		startLine := fmt.Sprintf("start\t%d\t%s\n", timestamp, w.url)
		_, err = w.gwriter.Write([]byte(startLine))
		if err != nil {
			return
		}
	} else {
		// this will flush and write gzip footer
		err = w.gwriter.Close()
		if err != nil {
			return
		}
		err = w.writer.Flush()
		if err != nil {
			return
		}

		// upload or store datasets
		err = w.uploadOrStore()
		if err != nil {
			return
		}
		// emptify buffer
		w.buffer.Reset()
		// don't have to do anything to writer
		// prepare gzip writer
		w.gwriter, err = gzip.NewWriterLevel(w.writer, gzip.BestCompression)
		if err != nil {
			return
		}

		if minute%10 == 0 {
			// if last digit of minute is 0 then write state snapshot
			var snapshots []simulator.Snapshot
			snapshots, err = w.sim.TakeStateSnapshot()
			for _, s := range snapshots {
				stateLine := fmt.Sprintf("state\t%d\t%s\t%s\n", timestamp, s.Channel, s.Snapshot)
				_, err = w.gwriter.Write([]byte(stateLine))
				if err != nil {
					return
				}
			}
		}
	}

	// change file timestamp, this is used to generate file name
	w.fileTimestamp = timestamp

	return
}

// this method assumes contents in buffer are complete
// this means it does not perform flush or closing gzip writer
// before writing the contents of buffer
func (w *Writer) uploadOrStore() (err error) {
	// name for file would be <exchange>_<timestamp>.gz
	fileName := fmt.Sprintf("%s_%d.gz", w.exchange, w.fileTimestamp)
	if !w.alwaysDisk {
		// try to upload it to s3
		// creating new reader from original buffer array because if you read bytes from
		// buffer, read bytes will be lost from buffer
		// we might use them later if s3 upload failed
		err = streamcommons.PutS3Object(fileName, bytes.NewReader(w.buffer.Bytes()))
		if err == nil {
			// successful
			w.logger.Println("uploaded to s3:", fileName)
			return
		}
		// if can not be uploaded to s3, then store it in local storage
		w.logger.Printf("Could not be uploaded to s3: %v\n", err)
	}
	// make directories to store file
	err = os.MkdirAll(w.directory, 0744)
	if err != nil {
		return
	}
	filePath := path.Join(w.directory, fileName)
	var file *os.File
	file, err = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY, 0744)
	if err != nil {
		return
	}
	defer func() {
		// defer function to ensure that opened file be closed
		serr := file.Close()
		if serr != nil {
			if err != nil {
				err = fmt.Errorf("%v, original error was: %v", serr, err)
			} else {
				err = serr
			}
		}
	}()

	_, err = file.Write(w.buffer.Bytes())

	w.logger.Printf("making new file: %s\n", fileName)
	return
}

// MessageChannelKnown writes message line to writer, but the channel is already known.
func (w *Writer) MessageChannelKnown(channel string, timestamp int64, message []byte) (err error) {
	w.lock.Lock()
	defer func() {
		w.lock.Unlock()
	}()
	timestamp, err = w.beforeWrite(timestamp)
	if err != nil {
		return
	}
	err = w.sim.ProcessMessageChannelKnown(channel, message)
	if err != nil {
		return
	}
	// write message despite the error (if happened)
	_, err = w.gwriter.Write([]byte(fmt.Sprintf("msg\t%d\t%s\t", timestamp, channel)))
	if err != nil {
		return
	}
	_, err = w.gwriter.Write(message)
	if err != nil {
		return
	}
	_, err = w.gwriter.Write([]byte("\n"))
	return
}

// Message writes msg line to writer. Channel is automatically determined.
func (w *Writer) Message(timestamp int64, message []byte) (err error) {
	// mark this writer is locked so routines in other thread will wait
	w.lock.Lock()
	defer func() {
		w.lock.Unlock()
	}()
	timestamp, err = w.beforeWrite(timestamp)
	if err != nil {
		return
	}
	var channel string
	channel, err = w.sim.ProcessMessageWebSocket(message)
	if err != nil {
		return
	}
	if channel == "" || channel == streamcommons.ChannelUnknown {
		// simulator could not determine the channel of message
		w.logger.Println("channel is unknown:", string(message))
	}
	// write message despite the error (if happened)
	_, err = w.gwriter.Write([]byte(fmt.Sprintf("msg\t%d\t%s\t", timestamp, channel)))
	if err != nil {
		return
	}
	_, err = w.gwriter.Write(message)
	if err != nil {
		return
	}
	_, err = w.gwriter.Write([]byte("\n"))
	if err != nil {
		return
	}
	return
}

// Send writes send line to writer. Channel is automatically determined.
func (w *Writer) Send(timestamp int64, message []byte) (err error) {
	w.lock.Lock()
	defer func() {
		w.lock.Unlock()
	}()
	timestamp, err = w.beforeWrite(timestamp)
	if err != nil {
		return
	}
	var channel string
	channel, err = w.sim.ProcessSend(message)
	if channel == "" || channel == streamcommons.ChannelUnknown {
		// simulator could not determine the channel of message
		w.logger.Println("channel is unknown:", string(message))
	}
	_, err = w.gwriter.Write([]byte(fmt.Sprintf("send\t%d\t%s\t", timestamp, channel)))
	if err != nil {
		return
	}
	_, err = w.gwriter.Write(message)
	if err != nil {
		return
	}
	_, err = w.gwriter.Write([]byte("\n"))
	return
}

// Error writes err line to writer.
func (w *Writer) Error(timestamp int64, message []byte) (err error) {
	w.lock.Lock()
	defer func() {
		w.lock.Unlock()
	}()
	timestamp, err = w.beforeWrite(timestamp)
	if err != nil {
		return
	}
	_, err = w.gwriter.Write([]byte(fmt.Sprintf("err\t%d\t%s\t\n", timestamp, message)))
	return
}

// Close closes this writer and underlying file and gzip writer. It also writes end line.
func (w *Writer) Close(timestamp int64) (err error) {
	w.lock.Lock()
	defer func() {
		w.lock.Unlock()
	}()
	// already closed
	if w.closed {
		return
	}
	timestamp, err = w.beforeWrite(timestamp)
	if err != nil {
		return
	}
	// report error as it is
	_, err = w.gwriter.Write([]byte(fmt.Sprintf("end\t%d\n", timestamp)))
	// this will also flush buffer in gzip writer
	serr := w.gwriter.Close()
	if serr != nil {
		if err != nil {
			err = fmt.Errorf("error on closing gzip: %v, previous error was: %v", serr, err)
		} else {
			err = fmt.Errorf("error on closing gzip: %v", serr)
		}
	}
	serr = w.writer.Flush()
	if serr != nil {
		if err != nil {
			err = fmt.Errorf("error on flushing writer: %v, previous error was: %v", serr, err)
		} else {
			err = fmt.Errorf("error on flushing writer: %v", serr)
		}
	}
	// don't forget to upload!
	serr = w.uploadOrStore()
	if serr != nil {
		if err != nil {
			err = fmt.Errorf("%v, previous error was: %v", serr, err)
		} else {
			err = serr
		}
	}
	w.closed = true
	return
}

// NewWriter creates new writer according to exchange given and returns it
// if error is reported, then there is no need to close returned writer
func NewWriter(exchange string, url string, directory string, alwaysDisk bool, logger *log.Logger) (w *Writer, err error) {
	w = new(Writer)
	w.exchange = exchange
	w.url = url
	w.directory = directory
	w.alwaysDisk = alwaysDisk
	w.sim, err = simulator.GetSimulator(exchange, nil)
	w.logger = logger
	return
}