package monitor

import (
	"strings"
	"testing"
	"time"
)

const splitTempMessage = "MSH|^~\\&|TR8|FAC|||||ORU^R01|1|P|2.4\r" +
	"OBX|10|NM|150344^MDC_TEMP^MDC|1.2.1.150344|18.3|268192^MDC_DIM_DEGC^MDC|30.0-40.0|||||20261008020630\r" +
	"OBX|11|NM|150344^MDC_TEMP^MDC|1.2.5.150344|36.4|268192^MDC_DIM_DEGC^MDC|30.0-40.0|||||20261008020630||APERIODIC\r"

func TestReassembler_MLLPAcrossDatagrams(t *testing.T) {
	framed := "\x0b" + splitTempMessage + "\x1c\r"
	cut1 := strings.Index(framed, "18.3") - 3
	cut2 := strings.Index(framed, "|150344^MDC_TEMP^MDC|1.2.5") + 5

	r := &hl7Reassembler{}
	now := time.Now()
	if out := r.Push([]byte(framed[:cut1]), now); len(out) != 0 {
		t.Fatalf("partial datagram produced %d messages", len(out))
	}
	if out := r.Push([]byte(framed[cut1:cut2]), now); len(out) != 0 {
		t.Fatalf("partial datagram produced %d messages", len(out))
	}
	out := r.Push([]byte(framed[cut2:]), now)
	if len(out) != 1 {
		t.Fatalf("expected 1 reassembled message, got %d", len(out))
	}
	msgs, err := ParseHL7Payload(out[0])
	if err != nil {
		t.Fatal(err)
	}
	if v := msgs[0].Vitals; v == nil || !v.Temp.Valid || v.Temp.Value != 36.4 {
		t.Fatalf("spot temp after reassembly: %+v info=%v", msgs[0].Vitals, msgs[0].Info)
	}
}

func TestReassembler_TwoFramedMessagesOneDatagram(t *testing.T) {
	data := "\x0b" + splitTempMessage + "\x1c\r\x0b" + splitTempMessage + "\x1c\r"
	r := &hl7Reassembler{}
	if out := r.Push([]byte(data), time.Now()); len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
}

func TestReassembler_UnframedSplitOnMSHAndIdleFlush(t *testing.T) {
	r := &hl7Reassembler{}
	now := time.Now()
	half := len(splitTempMessage) / 2
	if out := r.Push([]byte(splitTempMessage[:half]), now); len(out) != 0 {
		t.Fatalf("got %d messages from half a message", len(out))
	}
	out := r.Push([]byte(splitTempMessage[half:]+splitTempMessage), now)
	if len(out) != 1 {
		t.Fatalf("expected first message split off at next MSH, got %d", len(out))
	}
	if out := r.FlushIdle(now.Add(reassemblyIdleFlush / 2)); len(out) != 0 {
		t.Fatal("flushed before idle timeout")
	}
	if out := r.FlushIdle(now.Add(reassemblyIdleFlush)); len(out) != 1 {
		t.Fatalf("expected trailing message on idle flush, got %d", len(out))
	}
}
