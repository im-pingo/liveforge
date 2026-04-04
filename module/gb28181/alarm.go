package gb28181

import (
	"log/slog"

	"github.com/emiago/sipgo/sip"
)

// alarmHandler processes incoming alarm notifications.
type alarmHandler struct {
	registry *DeviceRegistry
}

// handleAlarm processes an alarm notification from a device MESSAGE.
func (a *alarmHandler) handleAlarm(alarm *AlarmNotify) {
	dev := a.registry.Get(alarm.DeviceID)
	if dev == nil {
		slog.Warn("alarm from unknown device", "module", "gb28181", "device", alarm.DeviceID)
		return
	}

	slog.Info("alarm notification", "module", "gb28181",
		"device", alarm.DeviceID,
		"method", alarm.AlarmMethod,
		"type", alarm.AlarmType,
		"time", alarm.AlarmTime)
}

// handleSubscribe handles SUBSCRIBE requests for alarm events.
func (a *alarmHandler) handleSubscribe(req *sip.Request, tx sip.ServerTransaction) {
	// Accept subscription — actual notification dispatching happens via MESSAGE.
	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	resp.AppendHeader(sip.NewHeader("Expires", "3600"))
	tx.Respond(resp)
}
