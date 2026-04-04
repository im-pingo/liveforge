package gb28181

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuildCatalogQuery(t *testing.T) {
	data := BuildCatalogQuery(1, "34020000001320000001")
	s := string(data)
	if !strings.Contains(s, "<CmdType>Catalog</CmdType>") {
		t.Error("missing CmdType")
	}
	if !strings.Contains(s, "<DeviceID>34020000001320000001</DeviceID>") {
		t.Error("missing DeviceID")
	}
	if !strings.Contains(s, "<?xml") {
		t.Error("missing XML declaration")
	}
}

func TestParseCatalogResponse(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<Response>
  <CmdType>Catalog</CmdType>
  <SN>1</SN>
  <DeviceID>34020000001320000001</DeviceID>
  <SumNum>2</SumNum>
  <DeviceList Num="2">
    <Item>
      <DeviceID>34020000001320000001</DeviceID>
      <Name>Camera 1</Name>
      <Manufacturer>Test</Manufacturer>
      <Status>ON</Status>
      <PTZType>1</PTZType>
      <Longitude>116.3</Longitude>
      <Latitude>39.9</Latitude>
    </Item>
    <Item>
      <DeviceID>34020000001320000002</DeviceID>
      <Name>Camera 2</Name>
      <Status>OFF</Status>
    </Item>
  </DeviceList>
</Response>`

	resp, err := ParseCatalogResponse([]byte(xmlData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.CmdType != "Catalog" {
		t.Errorf("CmdType = %q", resp.CmdType)
	}
	if resp.SumNum != 2 {
		t.Errorf("SumNum = %d", resp.SumNum)
	}
	if len(resp.DeviceList.Items) != 2 {
		t.Fatalf("Items len = %d", len(resp.DeviceList.Items))
	}
	if resp.DeviceList.Items[0].Name != "Camera 1" {
		t.Errorf("Item[0] Name = %q", resp.DeviceList.Items[0].Name)
	}
	if resp.DeviceList.Items[0].Longitude != 116.3 {
		t.Errorf("Longitude = %f", resp.DeviceList.Items[0].Longitude)
	}
}

func TestParseKeepalive(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<Notify>
  <CmdType>Keepalive</CmdType>
  <SN>5</SN>
  <DeviceID>34020000001320000001</DeviceID>
  <Status>OK</Status>
</Notify>`

	msg, err := ParseKeepalive([]byte(xmlData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.CmdType != "Keepalive" {
		t.Errorf("CmdType = %q", msg.CmdType)
	}
	if msg.DeviceID != "34020000001320000001" {
		t.Errorf("DeviceID = %q", msg.DeviceID)
	}
}

func TestParseAlarmNotify(t *testing.T) {
	xmlData := `<?xml version="1.0" encoding="UTF-8"?>
<Notify>
  <CmdType>Alarm</CmdType>
  <SN>1</SN>
  <DeviceID>34020000001320000001</DeviceID>
  <AlarmMethod>5</AlarmMethod>
  <AlarmType>2</AlarmType>
  <AlarmTime>2026-04-04T10:00:00</AlarmTime>
</Notify>`

	notify, err := ParseAlarmNotify([]byte(xmlData))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if notify.AlarmMethod != "5" {
		t.Errorf("AlarmMethod = %q", notify.AlarmMethod)
	}
}

func TestParseMessageType(t *testing.T) {
	tests := []struct {
		xml      string
		expected string
	}{
		{`<Notify><CmdType>Keepalive</CmdType></Notify>`, "Keepalive"},
		{`<Response><CmdType>Catalog</CmdType></Response>`, "Catalog"},
		{`<Notify><CmdType>Alarm</CmdType></Notify>`, "Alarm"},
		{`invalid xml`, ""},
	}
	for _, tt := range tests {
		got := ParseMessageType([]byte(tt.xml))
		if got != tt.expected {
			t.Errorf("ParseMessageType(%q) = %q, want %q", tt.xml, got, tt.expected)
		}
	}
}

func TestBuildPTZControl(t *testing.T) {
	data := BuildPTZControl(1, "34020000001320000001", "A50F01001F0000D6")
	s := string(data)
	if !strings.Contains(s, "<CmdType>DeviceControl</CmdType>") {
		t.Error("missing CmdType")
	}
	if !strings.Contains(s, "<PTZCmd>A50F01001F0000D6</PTZCmd>") {
		t.Error("missing PTZCmd")
	}
}

func TestCatalogQueryRoundTrip(t *testing.T) {
	q := CatalogQuery{
		CmdType:  "Catalog",
		SN:       42,
		DeviceID: "34020000001320000001",
	}
	data, err := xml.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var q2 CatalogQuery
	if err := xml.Unmarshal(data, &q2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if q2.SN != 42 {
		t.Errorf("SN = %d", q2.SN)
	}
}
