package monitor

import "testing"

func TestConvertTempToCelsius(t *testing.T) {
	c, ok := ConvertTempToCelsius(36.6, "MDC_DIM_DEGC")
	if !ok || c != 36.6 {
		t.Fatalf("degC: %v %v", c, ok)
	}
	c, ok = ConvertTempToCelsius(98.6, "MDC_DIM_DEGF")
	if !ok || c < 37 || c > 37.1 {
		t.Fatalf("degF: %v %v", c, ok)
	}
	c, ok = ConvertTempToCelsius(36.6, "")
	if !ok || c != 36.6 {
		t.Fatalf("missing unit assumes C: %v", c)
	}
}

func TestConvertWeightToKg(t *testing.T) {
	kg, ok := ConvertWeightToKg(80, "263441")
	if !ok || kg != 80 {
		t.Fatalf("kg: %v", kg)
	}
	kg, ok = ConvertWeightToKg(176.37, "MDC_DIM_LB")
	if !ok || kg < 79.9 || kg > 80.1 {
		t.Fatalf("lb: %v", kg)
	}
}

func TestConvertHeightToCm(t *testing.T) {
	cm, ok := ConvertHeightToCm(180, "MDC_DIM_CM")
	if !ok || cm != 180 {
		t.Fatalf("cm: %v", cm)
	}
	cm, ok = ConvertHeightToCm(70.87, "IN")
	if !ok || cm < 179 || cm > 181 {
		t.Fatalf("in: %v", cm)
	}
	cm, ok = ConvertHeightToCm(1.8, "M")
	if !ok || cm != 180 {
		t.Fatalf("m: %v", cm)
	}
}

func TestParseOBX_TempFahrenheit(t *testing.T) {
	msg := `MSH|^~\&|TR8|FAC|||||ORU^R01|1|P|2.4
OBR|1|||182777000^monitoring^SCT
OBX|1|NM|150344^MDC_TEMP^MDC||98.6|268224^MDC_DIM_DEGF^MDC`
	msgs, err := ParseHL7Payload([]byte(msg))
	if err != nil || len(msgs) != 1 {
		t.Fatal(err)
	}
	if !msgs[0].Vitals.Temp.Valid {
		t.Fatal("expected valid temp")
	}
	if msgs[0].Vitals.Temp.Value < 37 || msgs[0].Vitals.Temp.Value > 37.1 {
		t.Fatalf("temp %v", msgs[0].Vitals.Temp.Value)
	}
}
