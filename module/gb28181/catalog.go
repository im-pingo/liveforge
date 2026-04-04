package gb28181

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
	sipmod "github.com/im-pingo/liveforge/module/sip"
)

// catalogClient queries device catalogs via SIP MESSAGE.
type catalogClient struct {
	sipService sipmod.SIPService
	sn         atomic.Int64
}

// query sends a Catalog query to the given device.
func (c *catalogClient) query(ctx context.Context, device *Device) error {
	sn := int(c.sn.Add(1))
	body := BuildCatalogQuery(sn, device.DeviceID)

	serverID := c.sipService.ServerID()
	domain := c.sipService.Domain()
	remoteIP := extractIP(device.RemoteAddr)
	remotePort := parsePort(device.RemoteAddr)

	toURI := sip.Uri{
		User: device.DeviceID,
		Host: remoteIP,
		Port: remotePort,
	}

	req := sip.NewRequest(sip.MESSAGE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", serverID, domain, generateTag())))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", device.DeviceID, remoteIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody(body)

	resp, err := c.sipService.SendRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("send catalog query: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("catalog query rejected: %d %s", resp.StatusCode, resp.Reason)
	}

	slog.Info("catalog query sent", "module", "gb28181", "device", device.DeviceID, "sn", sn)
	return nil
}
