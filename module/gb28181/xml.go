package gb28181

import "encoding/xml"

// XMLMessage is the root element for GB28181 XML messages.
type XMLMessage struct {
	XMLName  xml.Name `xml:""`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
}

// KeepaliveMessage represents a Keepalive notification from a device.
type KeepaliveMessage struct {
	XMLName  xml.Name `xml:"Notify"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Status   string   `xml:"Status"`
}

// CatalogQuery represents a catalog query sent to a device.
type CatalogQuery struct {
	XMLName  xml.Name `xml:"Query"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
}

// CatalogResponse represents a catalog response from a device.
type CatalogResponse struct {
	XMLName  xml.Name      `xml:"Response"`
	CmdType  string        `xml:"CmdType"`
	SN       int           `xml:"SN"`
	DeviceID string        `xml:"DeviceID"`
	SumNum   int           `xml:"SumNum"`
	DeviceList CatalogDeviceList `xml:"DeviceList"`
}

// CatalogDeviceList wraps the list of items in a catalog response.
type CatalogDeviceList struct {
	Num   int           `xml:"Num,attr"`
	Items []CatalogItem `xml:"Item"`
}

// CatalogItem represents a single channel/device in a catalog response.
type CatalogItem struct {
	DeviceID     string  `xml:"DeviceID"`
	Name         string  `xml:"Name"`
	Manufacturer string  `xml:"Manufacturer"`
	Status       string  `xml:"Status"`
	PTZType      int     `xml:"PTZType"`
	Longitude    float64 `xml:"Longitude"`
	Latitude     float64 `xml:"Latitude"`
}

// DeviceInfoResponse represents a DeviceInfo response.
type DeviceInfoResponse struct {
	XMLName      xml.Name `xml:"Response"`
	CmdType      string   `xml:"CmdType"`
	SN           int      `xml:"SN"`
	DeviceID     string   `xml:"DeviceID"`
	DeviceName   string   `xml:"DeviceName"`
	Manufacturer string   `xml:"Manufacturer"`
	Model        string   `xml:"Model"`
	Firmware     string   `xml:"Firmware"`
}

// AlarmNotify represents an alarm notification from a device.
type AlarmNotify struct {
	XMLName     xml.Name `xml:"Notify"`
	CmdType     string   `xml:"CmdType"`
	SN          int      `xml:"SN"`
	DeviceID    string   `xml:"DeviceID"`
	AlarmMethod string   `xml:"AlarmMethod"`
	AlarmType   string   `xml:"AlarmType"`
	AlarmTime   string   `xml:"AlarmTime"`
}

// PTZControlMessage represents a PTZ control command sent to a device.
type PTZControlMessage struct {
	XMLName  xml.Name `xml:"Control"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	PTZCmd   string   `xml:"PTZCmd"`
}

// BuildCatalogQuery creates a catalog query XML body.
func BuildCatalogQuery(sn int, deviceID string) []byte {
	q := CatalogQuery{
		CmdType:  "Catalog",
		SN:       sn,
		DeviceID: deviceID,
	}
	data, _ := xml.MarshalIndent(q, "", "  ")
	return append([]byte(xml.Header), data...)
}

// BuildPTZControl creates a PTZ control XML body.
func BuildPTZControl(sn int, channelID, ptzCmd string) []byte {
	msg := PTZControlMessage{
		CmdType:  "DeviceControl",
		SN:       sn,
		DeviceID: channelID,
		PTZCmd:   ptzCmd,
	}
	data, _ := xml.MarshalIndent(msg, "", "  ")
	return append([]byte(xml.Header), data...)
}

// ParseMessageType detects the CmdType from an XML body.
func ParseMessageType(data []byte) string {
	var msg XMLMessage
	if err := xml.Unmarshal(data, &msg); err != nil {
		return ""
	}
	return msg.CmdType
}

// ParseKeepalive parses a keepalive message.
func ParseKeepalive(data []byte) (*KeepaliveMessage, error) {
	var msg KeepaliveMessage
	if err := xml.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ParseCatalogResponse parses a catalog response.
func ParseCatalogResponse(data []byte) (*CatalogResponse, error) {
	var resp CatalogResponse
	if err := xml.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ParseAlarmNotify parses an alarm notification.
func ParseAlarmNotify(data []byte) (*AlarmNotify, error) {
	var notify AlarmNotify
	if err := xml.Unmarshal(data, &notify); err != nil {
		return nil, err
	}
	return &notify, nil
}
