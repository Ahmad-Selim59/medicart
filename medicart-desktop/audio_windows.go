//go:build windows

package main

import (
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native WinMM (waveOut) PCM playback.
//
// Why not oto/WASAPI? oto/v3 activates the device through IAudioClient2,
// whose minimum supported client is Windows 8, and its WinMM fallback opens
// the device with WAVE_FORMAT_IEEE_FLOAT (32-bit float), which many older
// waveOut drivers reject. On an old 32-bit Windows box (commonly Win7) both
// paths fail and oto silently hands back a dead context — chunks are consumed
// and discarded with no sound and no surfaced error.
//
// waveOut with WAVE_FORMAT_PCM 16-bit is supported on essentially every
// Windows release and exactly matches the wire format we receive (s16le
// 16 kHz mono), so no conversion is needed. The Windows wave mapper
// (WAVE_MAPPER) transparently resamples to the device's native rate.

var (
	winmmDLL = windows.NewLazySystemDLL("winmm.dll")

	procWaveOutOpen            = winmmDLL.NewProc("waveOutOpen")
	procWaveOutClose           = winmmDLL.NewProc("waveOutClose")
	procWaveOutPrepareHeader   = winmmDLL.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepareHeader = winmmDLL.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite           = winmmDLL.NewProc("waveOutWrite")
	procWaveOutReset           = winmmDLL.NewProc("waveOutReset")
)

const (
	_WAVE_FORMAT_PCM = 1
	_WAVE_MAPPER     = 0xFFFFFFFF
	_CALLBACK_NULL   = 0
	_WHDR_DONE       = 0x00000001
	_WHDR_PREPARED   = 0x00000002

	// Number of reusable buffers. Each holds one paced frame, so this is the
	// device-side queue depth and therefore the playback latency floor:
	// 6 × 20 ms ≈ 120 ms of cushion, which absorbs normal network jitter while
	// keeping the delay small.
	winmmMaxHeaders   = 6
	winmmHeaderBufLen = 8192 // bytes; one paced frame (~640 B) fits easily
)

type _WAVEFORMATEX struct {
	wFormatTag      uint16
	nChannels       uint16
	nSamplesPerSec  uint32
	nAvgBytesPerSec uint32
	nBlockAlign     uint16
	wBitsPerSample  uint16
	cbSize          uint16
}

type _WAVEHDR struct {
	lpData          uintptr
	dwBufferLength  uint32
	dwBytesRecorded uint32
	dwUser          uintptr
	dwFlags         uint32
	dwLoops         uint32
	lpNext          uintptr
	reserved        uintptr
}

type winmmHeader struct {
	hdr      _WAVEHDR
	data     []byte
	prepared bool
	inUse    bool
}

type winmmPlayer struct {
	hwo     uintptr
	mu      sync.Mutex
	headers []*winmmHeader
	closed  bool
}

// newWindowsPCMPlayer opens the default output device for s16le PCM playback.
// The returned writer accepts raw little-endian 16-bit PCM samples.
func newWindowsPCMPlayer(sampleRate, channels, bitsPerSample int) (io.WriteCloser, error) {
	if err := winmmDLL.Load(); err != nil {
		return nil, fmt.Errorf("winmm.dll load failed: %w", err)
	}

	blockAlign := channels * bitsPerSample / 8
	f := _WAVEFORMATEX{
		wFormatTag:      _WAVE_FORMAT_PCM,
		nChannels:       uint16(channels),
		nSamplesPerSec:  uint32(sampleRate),
		nAvgBytesPerSec: uint32(sampleRate * blockAlign),
		nBlockAlign:     uint16(blockAlign),
		wBitsPerSample:  uint16(bitsPerSample),
		cbSize:          0,
	}

	var hwo uintptr
	r, _, _ := procWaveOutOpen.Call(
		uintptr(unsafe.Pointer(&hwo)),
		uintptr(_WAVE_MAPPER),
		uintptr(unsafe.Pointer(&f)),
		0, // dwCallback
		0, // dwInstance
		_CALLBACK_NULL,
	)
	if r != 0 {
		return nil, fmt.Errorf("waveOutOpen failed: mmsyserr=%d", r)
	}

	return &winmmPlayer{hwo: hwo}, nil
}

// Write queues PCM data for playback. It pages large writes through the
// fixed-size header buffers and blocks briefly only if every buffer is still
// playing (which, under the real-time paced sink, almost never happens).
func (p *winmmPlayer) Write(data []byte) (int, error) {
	total := len(data)
	for len(data) > 0 {
		n := len(data)
		if n > winmmHeaderBufLen {
			n = winmmHeaderBufLen
		}
		if err := p.writeOne(data[:n]); err != nil {
			return total - len(data), err
		}
		data = data[n:]
	}
	return total, nil
}

func (p *winmmPlayer) writeOne(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return io.ErrClosedPipe
	}

	h, err := p.acquireHeaderLocked()
	if err != nil {
		return err
	}

	copy(h.data, data)
	h.hdr.lpData = uintptr(unsafe.Pointer(&h.data[0]))
	h.hdr.dwBufferLength = uint32(len(data))

	if !h.prepared {
		r, _, _ := procWaveOutPrepareHeader.Call(p.hwo, uintptr(unsafe.Pointer(&h.hdr)), unsafe.Sizeof(h.hdr))
		if r != 0 {
			return fmt.Errorf("waveOutPrepareHeader failed: mmsyserr=%d", r)
		}
		h.prepared = true
	}

	r, _, _ := procWaveOutWrite.Call(p.hwo, uintptr(unsafe.Pointer(&h.hdr)), unsafe.Sizeof(h.hdr))
	if r != 0 {
		return fmt.Errorf("waveOutWrite failed: mmsyserr=%d", r)
	}
	h.inUse = true
	return nil
}

// acquireHeaderLocked returns a buffer that is free (never used or finished
// playing), allocating a new one up to winmmMaxHeaders. Must hold p.mu.
func (p *winmmPlayer) acquireHeaderLocked() (*winmmHeader, error) {
	for {
		for _, h := range p.headers {
			if h.inUse && h.hdr.dwFlags&_WHDR_DONE != 0 {
				h.inUse = false
			}
			if !h.inUse {
				return h, nil
			}
		}
		if len(p.headers) < winmmMaxHeaders {
			h := &winmmHeader{data: make([]byte, winmmHeaderBufLen)}
			p.headers = append(p.headers, h)
			return h, nil
		}
		// Every buffer is still playing. Wait for one to drain. Release the
		// lock so Close can interrupt us.
		p.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		p.mu.Lock()
		if p.closed {
			return nil, io.ErrClosedPipe
		}
	}
}

func (p *winmmPlayer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true

	procWaveOutReset.Call(p.hwo) // mark all queued buffers done
	for _, h := range p.headers {
		if h.prepared {
			procWaveOutUnprepareHeader.Call(p.hwo, uintptr(unsafe.Pointer(&h.hdr)), unsafe.Sizeof(h.hdr))
			h.prepared = false
		}
	}
	procWaveOutClose.Call(p.hwo)
	p.hwo = 0
	return nil
}
