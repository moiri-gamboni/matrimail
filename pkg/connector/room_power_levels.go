package connector

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Leicas/matrimail/pkg/matrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

// Refresh existing rooms too: changing creation defaults only fixes new threads.
func (ec *EmailConnector) refreshRoomPowerLevels(ctx context.Context) {
	portals, err := ec.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		ec.Bridge.Log.Error().Err(err).Msg("Failed to list email portals for power level refresh")
		return
	}
	updated := 0
	alreadyOK := 0
	failed := 0
	for _, portal := range portals {
		log := ec.Bridge.Log.With().Stringer("room_id", portal.MXID).Logger()
		log.Debug().Str("event_type", event.StatePowerLevels.Type).Str("state_key", "").Msg("GetStateEvent request")
		state, err := ec.Bridge.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState).GetStateEvent(ctx, portal.MXID, event.StatePowerLevels, "")
		log.Debug().Interface("state_event", state).Err(err).Msg("GetStateEvent response")
		if err != nil {
			log.Error().Err(err).Msg("Failed to get email room power levels")
			failed++
			continue
		}
		if state == nil {
			log.Error().Msg("Homeserver returned no email room power level event")
			failed++
			continue
		}
		log.Debug().Str("raw_content", string(state.Content.VeryRaw)).Msg("Email room power level content")
		current := &event.PowerLevelsEventContent{}
		// FullStateEvent ignores content parse errors, so decode the raw content explicitly.
		if err := json.Unmarshal(state.Content.VeryRaw, current); err != nil {
			log.Error().Err(err).Msg("Failed to parse email room power levels")
			failed++
			continue
		}
		botID := ec.Bridge.Bot.GetMXID()
		botLevel, hasBot := current.Users[botID]
		humanLevel := current.UsersDefault
		if login := ec.Bridge.GetCachedUserLoginByID(portal.Receiver); login != nil {
			humanLevel = current.GetUserLevel(login.User.MXID)
		}
		// FullStateEvent may populate the state cache; failed writes must not mutate it.
		power := current.Clone()
		overrides := matrix.BuildReadOnlyPowerLevels()
		changed := overrides.Apply(botID, power)
		// Rooms already in shape are the steady state; only a change is worth an info line.
		decision := log.Debug()
		if changed {
			decision = log.Info()
		}
		decision.Int("bot_level", botLevel).Int("human_level", humanLevel).
			Int("users_default", current.UsersDefault).
			Int("reaction_level", current.GetEventLevel(event.EventReaction)).Int("events_default", current.EventsDefault).
			Bool("changed", changed).Msg("Email room power level refresh decision")
		if !hasBot {
			log.Error().Interface("power_levels", current).Msg("Email room power levels have no bridge bot entry")
			failed++
			continue
		}
		if overrides.Apply("", power.Clone()) {
			log.Error().Msg("Bridge bot cannot apply required email room power levels")
			failed++
			continue
		}
		if !changed {
			alreadyOK++
			continue
		}
		content := &event.Content{Parsed: power}
		log.Debug().Interface("content", content).Msg("SendState m.room.power_levels request")
		response, err := ec.Bridge.Bot.SendState(ctx, portal.MXID, event.StatePowerLevels, "", content, time.Time{})
		log.Debug().Interface("response", response).Err(err).Msg("SendState m.room.power_levels response")
		if err != nil {
			log.Error().Err(err).Msg("Failed to update email room power levels")
			failed++
			continue
		}
		updated++
	}
	ec.Bridge.Log.Info().Int("portals", len(portals)).Int("updated", updated).Int("already_ok", alreadyOK).Int("failed", failed).Msg("Email room power level refresh complete")
}
