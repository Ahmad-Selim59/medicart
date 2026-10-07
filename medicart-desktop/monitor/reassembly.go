package monitor

import (
	"bytes"
	"time"
)

const (
	mllpStartBlock = 0x0B
	mllpEndBlock   = 0x1C

	// reassemblyIdleFlush is how long a partial buffer may sit without new datagrams
	// before it is parsed as-is (monitors that send unframed HL7).
	reassemblyIdleFlush = 400 * time.Millisecond
	reassemblyMaxBytes  = 4 << 20
)

// hl7Reassembler joins HL7 messages that the TR8 splits across several UDP datagrams.
// ZUG frames each message as <VT> message <FS><CR> (MLLP); unframed streams are split on MSH.
type hl7Reassembler struct {
	buf        []byte
	lastUpdate time.Time
	overflowed bool
}

// Push appends one datagram and returns every complete message now available.
func (r *hl7Reassembler) Push(data []byte, now time.Time) [][]byte {
	r.buf = append(r.buf, data...)
	r.lastUpdate = now
	r.overflowed = false
	if len(r.buf) > reassemblyMaxBytes {
		r.buf = nil
		r.overflowed = true
		return nil
	}
	return r.extract(false)
}

// FlushIdle returns the buffered remainder once no datagram has arrived for reassemblyIdleFlush.
func (r *hl7Reassembler) FlushIdle(now time.Time) [][]byte {
	if len(r.buf) == 0 || now.Sub(r.lastUpdate) < reassemblyIdleFlush {
		return nil
	}
	return r.extract(true)
}

// Overflowed reports whether the last Push discarded the buffer for exceeding reassemblyMaxBytes.
func (r *hl7Reassembler) Overflowed() bool {
	return r.overflowed
}

func (r *hl7Reassembler) extract(flushAll bool) [][]byte {
	var out [][]byte
	for {
		end := bytes.IndexByte(r.buf, mllpEndBlock)
		if end < 0 {
			break
		}
		if msg := trimMLLPStart(r.buf[:end]); len(msg) > 0 {
			out = append(out, msg)
		}
		rest := r.buf[end+1:]
		if len(rest) > 0 && rest[0] == '\r' {
			rest = rest[1:]
		}
		r.buf = append([]byte(nil), rest...)
	}

	// Without an MLLP start block we cannot rely on <FS>; a new MSH ends the previous message.
	if bytes.IndexByte(r.buf, mllpStartBlock) < 0 {
		for {
			first := bytes.Index(r.buf, []byte("MSH|"))
			if first < 0 {
				break
			}
			next := bytes.Index(r.buf[first+4:], []byte("MSH|"))
			if next < 0 {
				break
			}
			next += first + 4
			if msg := bytes.TrimSpace(r.buf[first:next]); len(msg) > 0 {
				out = append(out, msg)
			}
			r.buf = append([]byte(nil), r.buf[next:]...)
		}
	}

	if flushAll && len(r.buf) > 0 {
		if msg := trimMLLPStart(r.buf); len(bytes.TrimSpace(msg)) > 0 {
			out = append(out, msg)
		}
		r.buf = nil
	}
	return out
}

func trimMLLPStart(b []byte) []byte {
	if i := bytes.IndexByte(b, mllpStartBlock); i >= 0 {
		b = b[i+1:]
	}
	return bytes.TrimSpace(b)
}
