// Copyright 2017 Vector Creations Ltd
// Copyright 2018 New Vector Ltd
// Copyright 2019-2020 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package input

import (
	"bytes"
	"context"
	"fmt"
	"time"

	fedapi "github.com/matrix-org/dendrite/federationapi/api"
	"github.com/matrix-org/dendrite/internal/eventutil"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/roomserver/internal/helpers"
	"github.com/matrix-org/dendrite/roomserver/state"
	"github.com/matrix-org/dendrite/roomserver/types"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

func init() {
	prometheus.MustRegister(processRoomEventDuration)
}

var processRoomEventDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "dendrite",
		Subsystem: "roomserver",
		Name:      "processroomevent_duration_millis",
		Help:      "How long it takes the roomserver to process an event",
		Buckets: []float64{ // milliseconds
			5, 10, 25, 50, 75, 100, 250, 500,
			1000, 2000, 3000, 4000, 5000, 6000,
			7000, 8000, 9000, 10000, 15000, 20000,
		},
	},
	[]string{"room_id"},
)

// processRoomEvent can only be called once at a time
//
// TODO(#375): This should be rewritten to allow concurrent calls. The
// difficulty is in ensuring that we correctly annotate events with the correct
// state deltas when sending to kafka streams
// TODO: Break up function - we should probably do transaction ID checks before calling this.
// nolint:gocyclo
func (r *Inputer) processRoomEvent(
	ctx context.Context,
	input *api.InputRoomEvent,
) (string, error) {
	// Measure how long it takes to process this event.
	started := time.Now()
	defer func() {
		timetaken := time.Since(started)
		processRoomEventDuration.With(prometheus.Labels{
			"room_id": input.Event.RoomID(),
		}).Observe(float64(timetaken.Milliseconds()))
	}()

	// Parse and validate the event JSON
	headered := input.Event
	event := headered.Unwrap()

	// if we have already got this event then do not process it again, if the input kind is an outlier.
	// Outliers contain no extra information which may warrant a re-processing.
	if input.Kind == api.KindOutlier {
		evs, err2 := r.DB.EventsFromIDs(ctx, []string{event.EventID()})
		if err2 == nil && len(evs) == 1 {
			// check hash matches if we're on early room versions where the event ID was a random string
			idFormat, err2 := headered.RoomVersion.EventIDFormat()
			if err2 == nil {
				switch idFormat {
				case gomatrixserverlib.EventIDFormatV1:
					if bytes.Equal(event.EventReference().EventSHA256, evs[0].EventReference().EventSHA256) {
						util.GetLogger(ctx).WithField("event_id", event.EventID()).Infof("Already processed event; ignoring")
						return event.EventID(), nil
					}
				default:
					util.GetLogger(ctx).WithField("event_id", event.EventID()).Infof("Already processed event; ignoring")
					return event.EventID(), nil
				}
			}
		}
	}

	// First of all, check that the auth events of the event are known.
	// If they aren't then we will ask the federation API for them.
	if err := r.checkForMissingAuthEvents(ctx, input.Event, map[string]types.EventNID{}); err != nil {
		logrus.WithError(err).Error("XXX: r.checkForMissingAuthEvents")
		return "", fmt.Errorf("r.checkForMissingAuthEvents: %w", err)
	}

	// Check that the event passes authentication checks and work out
	// the numeric IDs for the auth events.
	isRejected := false
	authEventNIDs, rejectionErr := helpers.CheckAuthEvents(ctx, r.DB, headered, input.AuthEventIDs)
	if rejectionErr != nil {
		logrus.WithError(rejectionErr).WithField("event_id", event.EventID()).WithField("auth_event_ids", input.AuthEventIDs).Error("helpers.CheckAuthEvents failed for event, rejecting event")
		isRejected = true
	}

	// Then check if the prev events are known, which we need in order
	// to calculate the state before the event.
	if err := r.checkForMissingPrevEvents(ctx, input); err != nil {
		return "", fmt.Errorf("r.checkForMissingPrevEvents: %w", err)
	}

	var softfail bool
	if input.Kind == api.KindNew {
		// Check that the event passes authentication checks based on the
		// current room state.
		var err error
		softfail, err = helpers.CheckForSoftFail(ctx, r.DB, headered, input.StateEventIDs)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"event_id": event.EventID(),
				"type":     event.Type(),
				"room":     event.RoomID(),
			}).WithError(err).Info("Error authing soft-failed event")
		}
	}

	// Store the event.
	_, stateAtEvent, redactionEvent, redactedEventID, err := r.DB.StoreEvent(ctx, event, authEventNIDs, isRejected)
	if err != nil {
		return "", fmt.Errorf("r.DB.StoreEvent: %w", err)
	}

	// if storing this event results in it being redacted then do so.
	if !isRejected && redactedEventID == event.EventID() {
		r, rerr := eventutil.RedactEvent(redactionEvent, event)
		if rerr != nil {
			return "", fmt.Errorf("eventutil.RedactEvent: %w", rerr)
		}
		event = r
	}

	// For outliers we can stop after we've stored the event itself as it
	// doesn't have any associated state to store and we don't need to
	// notify anyone about it.
	if input.Kind == api.KindOutlier {
		logrus.WithFields(logrus.Fields{
			"event_id": event.EventID(),
			"type":     event.Type(),
			"room":     event.RoomID(),
			"sender":   event.Sender(),
		}).Debug("Stored outlier")
		return event.EventID(), nil
	}

	roomInfo, err := r.DB.RoomInfo(ctx, event.RoomID())
	if err != nil {
		return "", fmt.Errorf("r.DB.RoomInfo: %w", err)
	}
	if roomInfo == nil {
		return "", fmt.Errorf("r.DB.RoomInfo missing for room %s", event.RoomID())
	}

	if stateAtEvent.BeforeStateSnapshotNID == 0 {
		// We haven't calculated a state for this event yet.
		// Lets calculate one.
		err = r.calculateAndSetState(ctx, input, *roomInfo, &stateAtEvent, event, isRejected)
		if err != nil && input.Kind != api.KindOld {
			return "", fmt.Errorf("r.calculateAndSetState: %w", err)
		}
	}

	// We stop here if the event is rejected: We've stored it but won't update forward extremities or notify anyone about it.
	if isRejected || softfail {
		logrus.WithFields(logrus.Fields{
			"event_id":  event.EventID(),
			"type":      event.Type(),
			"room":      event.RoomID(),
			"soft_fail": softfail,
			"sender":    event.Sender(),
		}).Debug("Stored rejected event")
		return event.EventID(), rejectionErr
	}

	switch input.Kind {
	case api.KindNew:
		if err = r.updateLatestEvents(
			ctx,                 // context
			roomInfo,            // room info for the room being updated
			stateAtEvent,        // state at event (below)
			event,               // event
			input.SendAsServer,  // send as server
			input.TransactionID, // transaction ID
			input.HasState,      // rewrites state?
		); err != nil {
			return "", fmt.Errorf("r.updateLatestEvents: %w", err)
		}
	case api.KindOld:
		err = r.WriteOutputEvents(event.RoomID(), []api.OutputEvent{
			{
				Type: api.OutputTypeOldRoomEvent,
				OldRoomEvent: &api.OutputOldRoomEvent{
					Event: headered,
				},
			},
		})
		if err != nil {
			return "", fmt.Errorf("r.WriteOutputEvents (old): %w", err)
		}
	}

	// processing this event resulted in an event (which may not be the one we're processing)
	// being redacted. We are guaranteed to have both sides (the redaction/redacted event),
	// so notify downstream components to redact this event - they should have it if they've
	// been tracking our output log.
	if redactedEventID != "" {
		err = r.WriteOutputEvents(event.RoomID(), []api.OutputEvent{
			{
				Type: api.OutputTypeRedactedEvent,
				RedactedEvent: &api.OutputRedactedEvent{
					RedactedEventID: redactedEventID,
					RedactedBecause: redactionEvent.Headered(headered.RoomVersion),
				},
			},
		})
		if err != nil {
			return "", fmt.Errorf("r.WriteOutputEvents (redactions): %w", err)
		}
	}

	// Update the extremities of the event graph for the room
	return event.EventID(), nil
}

func (r *Inputer) checkForMissingAuthEvents(
	ctx context.Context,
	event *gomatrixserverlib.HeaderedEvent,
	cache map[string]types.EventNID,
) error {
	authEventIDs := event.AuthEventIDs()
	if len(authEventIDs) == 0 {
		return nil
	}

	knownAuthEventNIDs, err := r.DB.EventNIDs(ctx, authEventIDs)
	if err != nil {
		return fmt.Errorf("r.DB.EventNIDs: %w", err)
	}
	for authEventID, authEventNID := range knownAuthEventNIDs {
		cache[authEventID] = authEventNID
	}

	missingAuthEventIDs := make([]string, 0, len(authEventIDs)-len(knownAuthEventNIDs))
	for _, authEventID := range authEventIDs {
		if _, ok := knownAuthEventNIDs[authEventID]; !ok {
			missingAuthEventIDs = append(missingAuthEventIDs, authEventID)
		}
	}

	if len(missingAuthEventIDs) > 0 {
		req := &fedapi.QueryEventAuthFromFederationRequest{
			RoomID:  event.RoomID(),
			EventID: event.EventID(),
		}
		res := &fedapi.QueryEventAuthFromFederationResponse{}
		if err := r.FSAPI.QueryEventAuthFromFederation(ctx, req, res); err != nil {
			return fmt.Errorf("r.FSAPI.QueryEventAuthFromFederation: %w", err)
		}

		for _, event := range gomatrixserverlib.ReverseTopologicalOrdering(
			res.Events,
			gomatrixserverlib.TopologicalOrderByAuthEvents,
		) {
			// Work out which event NIDs we need to look up from the database. If
			// the event NID is already in the event map in memory then we can don't
			// need to ask the database again.
			neededAuthEventNIDs := make([]string, 0, len(event.AuthEventIDs()))
			for _, authEventID := range event.AuthEventIDs() {
				if _, ok := cache[authEventID]; !ok {
					neededAuthEventNIDs = append(neededAuthEventNIDs, authEventID)
				}
			}

			// If we need to fetch some event NIDs from the database then do that.
			// We will also add those to the auth event map in memory, so that we
			// can skip future database hits for the same event IDs.
			if len(neededAuthEventNIDs) > 0 {
				newAuthEventNIDs, err := r.DB.EventNIDs(ctx, neededAuthEventNIDs)
				if err != nil {
					return fmt.Errorf("r.DB.EventNIDs: %w", err)
				}
				for authEventID, authEventNID := range newAuthEventNIDs {
					cache[authEventID] = authEventNID
				}
			}

			// Now collect the event NIDs for all of the auth events.
			authEventNIDsForEvent := make([]types.EventNID, 0, len(event.AuthEventIDs()))
			for _, authEventID := range event.AuthEventIDs() {
				authEventNIDsForEvent = append(authEventNIDsForEvent, cache[authEventID])
			}

			// If we haven't accumulated all of the auth events needed for the
			// event then we shouldn't persist the event as something is wrong.
			if len(authEventNIDsForEvent) != len(event.AuthEventIDs()) {
				return fmt.Errorf("missing auth event NIDs for event %s", event.EventID())
			}

			// Finally, store the event in the database.
			if _, _, _, _, err := r.DB.StoreEvent(ctx, event, authEventNIDsForEvent, false); err != nil {
				return fmt.Errorf("r.DB.StoreEvent: %w", err)
			}
		}
	}

	return nil
}

func (r *Inputer) checkForMissingPrevEvents(
	ctx context.Context,
	input *api.InputRoomEvent,
) error {

	return nil
}

func (r *Inputer) calculateAndSetState(
	ctx context.Context,
	input *api.InputRoomEvent,
	roomInfo types.RoomInfo,
	stateAtEvent *types.StateAtEvent,
	event *gomatrixserverlib.Event,
	isRejected bool,
) error {
	var err error
	roomState := state.NewStateResolution(r.DB, roomInfo)

	if input.HasState && !isRejected {
		// Check here if we think we're in the room already.
		stateAtEvent.Overwrite = true
		var joinEventNIDs []types.EventNID
		// Request join memberships only for local users only.
		if joinEventNIDs, err = r.DB.GetMembershipEventNIDsForRoom(ctx, roomInfo.RoomNID, true, true); err == nil {
			// If we have no local users that are joined to the room then any state about
			// the room that we have is quite possibly out of date. Therefore in that case
			// we should overwrite it rather than merge it.
			stateAtEvent.Overwrite = len(joinEventNIDs) == 0
		}

		// We've been told what the state at the event is so we don't need to calculate it.
		// Check that those state events are in the database and store the state.
		var entries []types.StateEntry
		if entries, err = r.DB.StateEntriesForEventIDs(ctx, input.StateEventIDs); err != nil {
			return fmt.Errorf("r.DB.StateEntriesForEventIDs: %w", err)
		}
		entries = types.DeduplicateStateEntries(entries)

		if stateAtEvent.BeforeStateSnapshotNID, err = r.DB.AddState(ctx, roomInfo.RoomNID, nil, entries); err != nil {
			return fmt.Errorf("r.DB.AddState: %w", err)
		}
	} else {
		stateAtEvent.Overwrite = false

		// We haven't been told what the state at the event is so we need to calculate it from the prev_events
		if stateAtEvent.BeforeStateSnapshotNID, err = roomState.CalculateAndStoreStateBeforeEvent(ctx, event, isRejected); err != nil {
			return fmt.Errorf("roomState.CalculateAndStoreStateBeforeEvent: %w", err)
		}
	}

	err = r.DB.SetState(ctx, stateAtEvent.EventNID, stateAtEvent.BeforeStateSnapshotNID)
	if err != nil {
		return fmt.Errorf("r.DB.SetState: %w", err)
	}
	return nil
}
