package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Leicas/matrimail/pkg/matrix"
	"maunium.net/go/mautrix/event"
)

// Refresh existing rooms too: changing creation defaults only fixes new threads.
func (ec *EmailConnector) refreshRoomPowerLevels(ctx context.Context) error {
	portals, err := ec.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return fmt.Errorf("list email portals for power level refresh: %w", err)
	}
	updated := 0
	var failures []error
	for _, portal := range portals {
		log := ec.Bridge.Log.With().Stringer("room_id", portal.MXID).Logger()
		log.Debug().Msg("GetPowerLevels request")
		current, err := ec.Bridge.Matrix.GetPowerLevels(ctx, portal.MXID)
		log.Debug().Interface("power_levels", current).Err(err).Msg("GetPowerLevels response")
		if err != nil {
			failures = append(failures, fmt.Errorf("get power levels for %s: %w", portal.MXID, err))
			continue
		}
		// The framework may return cached state. Failed writes must not change it.
		power := current.Clone()
		if !matrix.BuildReadOnlyPowerLevels().Apply(ec.Bridge.Bot.GetMXID(), power) {
			continue
		}
		content := &event.Content{Parsed: power}
		log.Debug().Interface("content", content).Msg("SendState m.room.power_levels request")
		response, err := ec.Bridge.Bot.SendState(ctx, portal.MXID, event.StatePowerLevels, "", content, time.Time{})
		log.Debug().Interface("response", response).Err(err).Msg("SendState m.room.power_levels response")
		if err != nil {
			failures = append(failures, fmt.Errorf("set power levels for %s: %w", portal.MXID, err))
			continue
		}
		updated++
	}
	ec.Bridge.Log.Info().Int("portals", len(portals)).Int("updated", updated).Int("failed", len(failures)).Msg("Email room power level refresh complete")
	// Finish the batch so one inaccessible room does not strand the other rooms.
	return errors.Join(failures...)
}
