package main

import (
	"encoding/xml"
	"reflect"
	"testing"
)

func TestBuildChannelsDerivesSingleCameraMetadata(t *testing.T) {
	got := buildChannels("34020000001110001234")
	if len(got) != 1 {
		t.Fatalf("channels=%d want=1", len(got))
	}
	want := channelInfo{
		ID:           "34020000001110001234",
		Name:         "Camera 1234",
		Manufacturer: "Hikvision",
		Status:       "ON",
		PTZType:      1,
		Latitude:     39.916527,
		Longitude:    116.397128,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("channel=%+v want=%+v", got[0], want)
	}
}

func TestBuildCatalogResponsePreservesQueryAndChannelFields(t *testing.T) {
	previousChannels := channels
	previousDeviceID := *flagDeviceID
	t.Cleanup(func() {
		channels = previousChannels
		*flagDeviceID = previousDeviceID
	})
	*flagDeviceID = "34020000001110000001"
	channels = []channelInfo{
		{
			ID:           "34020000001320000001",
			Name:         "Entrance",
			Manufacturer: "Acme",
			Status:       "ON",
			PTZType:      2,
			Latitude:     31.2304,
			Longitude:    121.4737,
		},
	}

	type catalogResponse struct {
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
		SumNum   int    `xml:"SumNum"`
		List     struct {
			Num   int `xml:"Num,attr"`
			Items []struct {
				DeviceID     string  `xml:"DeviceID"`
				Name         string  `xml:"Name"`
				Manufacturer string  `xml:"Manufacturer"`
				Status       string  `xml:"Status"`
				PTZType      int     `xml:"PTZType"`
				Longitude    float64 `xml:"Longitude"`
				Latitude     float64 `xml:"Latitude"`
			} `xml:"Item"`
		} `xml:"DeviceList"`
	}

	body := buildCatalogResponse([]byte(`<Query><CmdType>Catalog</CmdType><SN>73</SN><DeviceID>request-target</DeviceID></Query>`))
	var got catalogResponse
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal catalog response: %v\n%s", err, body)
	}
	if got.CmdType != "Catalog" || got.SN != 73 || got.DeviceID != "34020000001110000001" {
		t.Fatalf("catalog header=%+v", got)
	}
	if got.SumNum != 1 || got.List.Num != 1 || len(got.List.Items) != 1 {
		t.Fatalf("catalog counts sum=%d num=%d items=%d", got.SumNum, got.List.Num, len(got.List.Items))
	}
	want := channels[0]
	item := got.List.Items[0]
	if item.DeviceID != want.ID || item.Name != want.Name || item.Manufacturer != want.Manufacturer ||
		item.Status != want.Status || item.PTZType != want.PTZType ||
		item.Longitude != want.Longitude || item.Latitude != want.Latitude {
		t.Fatalf("catalog item=%+v want=%+v", item, want)
	}
}

func TestParseCmdType(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "catalog", body: `<Query><CmdType>Catalog</CmdType></Query>`, want: "Catalog"},
		{name: "control", body: `<Control><CmdType>DeviceControl</CmdType></Control>`, want: "DeviceControl"},
		{name: "malformed", body: `<Query>`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCmdType([]byte(tc.body)); got != tc.want {
				t.Fatalf("parseCmdType()=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestParseSDPPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		sdp  string
		want int
	}{
		{name: "video", sdp: "v=0\r\nc=IN IP4 127.0.0.1\r\nm=video 40002 RTP/AVP 96\r\n", want: 40002},
		{name: "missing", sdp: "v=0\r\nc=IN IP4 127.0.0.1\r\n", want: 0},
		{name: "invalid", sdp: "m=video invalid RTP/AVP 96\r\n", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSDPPort(tc.sdp); got != tc.want {
				t.Fatalf("parseSDPPort()=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestAddressParsing(t *testing.T) {
	for _, tc := range []struct {
		addr     string
		wantIP   string
		wantPort int
	}{
		{addr: "192.0.2.10:5070", wantIP: "192.0.2.10", wantPort: 5070},
		{addr: "[2001:db8::1]:5080", wantIP: "2001:db8::1", wantPort: 5080},
		{addr: "example.test", wantIP: "example.test", wantPort: 5060},
		{addr: "example.test:0", wantIP: "example.test", wantPort: 5060},
	} {
		if got := extractIP(tc.addr); got != tc.wantIP {
			t.Errorf("extractIP(%q)=%q want=%q", tc.addr, got, tc.wantIP)
		}
		if got := parseAddrPort(tc.addr); got != tc.wantPort {
			t.Errorf("parseAddrPort(%q)=%d want=%d", tc.addr, got, tc.wantPort)
		}
	}
}

func TestSplitOneAccessUnitAtAnnexBBoundary(t *testing.T) {
	data := []byte{
		0, 0, 0, 1, 0x67, 0x11,
		0, 0, 1, 0x65, 0x22,
		0, 0, 0, 1, 0x41, 0x33,
	}
	wantAU := data[:11]
	wantRest := data[11:]
	au, rest, found := splitOneAccessUnit(data)
	if !found {
		t.Fatal("expected a complete access unit")
	}
	if !reflect.DeepEqual(au, wantAU) {
		t.Fatalf("access unit=%v want=%v", au, wantAU)
	}
	if !reflect.DeepEqual(rest, wantRest) {
		t.Fatalf("remaining bytes=%v want=%v", rest, wantRest)
	}
}
