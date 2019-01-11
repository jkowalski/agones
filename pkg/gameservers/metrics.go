// Copyright 2018 Google Inc. All Rights Reserved.
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

package gameservers

import (
	"fmt"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
)

var (
	keyFleetName        = mustTagKey("fleet_name")
	keyGameServerStatus = mustTagKey("status")

	gameServerEnqueueRate    = stats.Int64("gameservers/controller_enqueues", "The count of GS controller enqueues per fleet", "1")
	gameServerDequeueRate    = stats.Int64("gameservers/controller_dequeues", "The count of GS controller Dequeues per fleet", "1")
	gameServerSyncTimeMillis = stats.Int64("gameservers/sync_time_millis", "Game server sync time by status", "ms")
)

func init() {
	mustRegister(&view.View{
		Name:        "controller_dequeues",
		Measure:     gameServerDequeueRate,
		Description: "Number of GS Dequeues per fleet",
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{keyFleetName},
	})
	mustRegister(&view.View{
		Name:        "controller_enqueues",
		Measure:     gameServerEnqueueRate,
		Description: "Number of GS enqueues per fleet",
		Aggregation: view.Count(),
		TagKeys:     []tag.Key{keyFleetName},
	})
	mustRegister(&view.View{
		Name:        "gameserver_sync_time_usec",
		Description: "distribution of game server sync time in milliseconds",
		Measure:     gameServerSyncTimeMillis,
		Aggregation: view.Distribution(0, 1, 5, 10, 50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000),
		TagKeys:     []tag.Key{keyGameServerStatus},
	})
}

func mustRegister(v *view.View) {
	if err := view.Register(v); err != nil {
		panic(fmt.Sprintf("Failed to register view: %v", err))
	}
}

func mustTagKey(key string) tag.Key {
	t, err := tag.NewKey(key)
	if err != nil {
		panic(err)
	}
	return t
}
