// Copyright 2020 The Matrix.org Foundation C.I.C.
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

//go:build wasm
// +build wasm

package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"syscall/js"

	"github.com/gorilla/mux"
	"github.com/matrix-org/dendrite/appservice"
	"github.com/matrix-org/dendrite/cmd/dendrite-demo-pinecone/conn"
	"github.com/matrix-org/dendrite/cmd/dendrite-demo-pinecone/rooms"
	"github.com/matrix-org/dendrite/cmd/dendrite-demo-yggdrasil/signing"
	"github.com/matrix-org/dendrite/federationapi"
	"github.com/matrix-org/dendrite/internal/caching"
	"github.com/matrix-org/dendrite/internal/httputil"
	"github.com/matrix-org/dendrite/internal/sqlutil"
	"github.com/matrix-org/dendrite/roomserver"
	"github.com/matrix-org/dendrite/setup"
	"github.com/matrix-org/dendrite/setup/config"
	"github.com/matrix-org/dendrite/setup/jetstream"
	"github.com/matrix-org/dendrite/setup/process"
	"github.com/matrix-org/dendrite/userapi"

	"github.com/matrix-org/gomatrixserverlib"

	"github.com/sirupsen/logrus"

	_ "github.com/matrix-org/go-sqlite3-js"

	pineconeConnections "github.com/matrix-org/pinecone/connections"
	pineconeRouter "github.com/matrix-org/pinecone/router"
	pineconeSessions "github.com/matrix-org/pinecone/sessions"
)

var GitCommit string

func init() {
	fmt.Printf("[%s] dendrite.js starting...\n", GitCommit)
}

const publicPeer = "wss://pinecone.matrix.org/public"
const keyNameEd25519 = "_go_ed25519_key"

func readKeyFromLocalStorage() (key ed25519.PrivateKey, err error) {
	localforage := js.Global().Get("localforage")
	if !localforage.Truthy() {
		err = fmt.Errorf("readKeyFromLocalStorage: no localforage")
		return
	}
	// https://localforage.github.io/localForage/
	item, ok := await(localforage.Call("getItem", keyNameEd25519))
	if !ok || !item.Truthy() {
		err = fmt.Errorf("readKeyFromLocalStorage: no key in localforage")
		return
	}
	fmt.Println("Found key in localforage")
	// extract []byte and make an ed25519 key
	seed := make([]byte, 32, 32)
	js.CopyBytesToGo(seed, item)

	return ed25519.NewKeyFromSeed(seed), nil
}

func writeKeyToLocalStorage(key ed25519.PrivateKey) error {
	localforage := js.Global().Get("localforage")
	if !localforage.Truthy() {
		return fmt.Errorf("writeKeyToLocalStorage: no localforage")
	}

	// make a Uint8Array from the key's seed
	seed := key.Seed()
	jsSeed := js.Global().Get("Uint8Array").New(len(seed))
	js.CopyBytesToJS(jsSeed, seed)
	// write it
	localforage.Call("setItem", keyNameEd25519, jsSeed)
	return nil
}

// taken from https://go-review.googlesource.com/c/go/+/150917

// await waits until the promise v has been resolved or rejected and returns the promise's result value.
// The boolean value ok is true if the promise has been resolved, false if it has been rejected.
// If v is not a promise, v itself is returned as the value and ok is true.
func await(v js.Value) (result js.Value, ok bool) {
	if v.Type() != js.TypeObject || v.Get("then").Type() != js.TypeFunction {
		return v, true
	}
	done := make(chan struct{})
	onResolve := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		result = args[0]
		ok = true
		close(done)
		return nil
	})
	defer onResolve.Release()
	onReject := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		result = args[0]
		ok = false
		close(done)
		return nil
	})
	defer onReject.Release()
	v.Call("then", onResolve, onReject)
	<-done
	return
}

func generateKey() ed25519.PrivateKey {
	// attempt to look for a seed in JS-land and if it exists use it.
	priv, err := readKeyFromLocalStorage()
	if err == nil {
		fmt.Println("Read key from localStorage")
		return priv
	}
	// generate a new key
	fmt.Println(err, " : Generating new ed25519 key")
	_, priv, err = ed25519.GenerateKey(nil)
	if err != nil {
		logrus.Fatalf("Failed to generate ed25519 key: %s", err)
	}
	if err := writeKeyToLocalStorage(priv); err != nil {
		fmt.Println("failed to write key to localStorage: ", err)
		// non-fatal, we'll just have amnesia for a while
	}
	return priv
}

func main() {
	startup()

	// We want to block forever to let the fetch and libp2p handler serve the APIs
	select {}
}

func startup() {
	sk := generateKey()
	pk := sk.Public().(ed25519.PublicKey)

	pRouter := pineconeRouter.NewRouter(logrus.WithField("pinecone", "router"), sk, false)
	pSessions := pineconeSessions.NewSessions(logrus.WithField("pinecone", "sessions"), pRouter, []string{"matrix"})
	pManager := pineconeConnections.NewConnectionManager(pRouter)
	pManager.AddPeer("wss://pinecone.matrix.org/public")

	cfg := &config.Dendrite{}
	cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: false})
	cfg.UserAPI.AccountDatabase.ConnectionString = "file:/idb/dendritejs_account.db"
	cfg.FederationAPI.Database.ConnectionString = "file:/idb/dendritejs_fedsender.db"
	cfg.MediaAPI.Database.ConnectionString = "file:/idb/dendritejs_mediaapi.db"
	cfg.RoomServer.Database.ConnectionString = "file:/idb/dendritejs_roomserver.db"
	cfg.SyncAPI.Database.ConnectionString = "file:/idb/dendritejs_syncapi.db"
	cfg.KeyServer.Database.ConnectionString = "file:/idb/dendritejs_e2ekey.db"
	cfg.Global.JetStream.StoragePath = "file:/idb/dendritejs/"
	cfg.Global.TrustedIDServers = []string{}
	cfg.Global.KeyID = gomatrixserverlib.KeyID(signing.KeyID)
	cfg.Global.PrivateKey = sk
	cfg.Global.ServerName = spec.ServerName(hex.EncodeToString(pk))
	cfg.ClientAPI.RegistrationDisabled = false
	cfg.ClientAPI.OpenRegistrationWithoutVerificationEnabled = true

	if err := cfg.Derive(); err != nil {
		logrus.Fatalf("Failed to derive values from config: %s", err)
	}
	natsInstance := jetstream.NATSInstance{}
	processCtx := process.NewProcessContext()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	routers := httputil.NewRouters()
	caches := caching.NewRistrettoCache(cfg.Global.Cache.EstimatedMaxSize, cfg.Global.Cache.MaxAge, caching.EnableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.EnableMetrics)

	federation := conn.CreateFederationClient(cfg, pSessions)

	serverKeyAPI := &signing.YggdrasilKeys{}
	keyRing := serverKeyAPI.KeyRing()

	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, federation)

	asQuery := appservice.NewInternalAPI(
		processCtx, cfg, &natsInstance, userAPI, rsAPI,
	)
	rsAPI.SetAppserviceAPI(asQuery)
	fedSenderAPI := federationapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, federation, rsAPI, caches, keyRing, true)
	rsAPI.SetFederationAPI(fedSenderAPI, keyRing)

	monolith := setup.Monolith{
		Config:    cfg,
		Client:    conn.CreateClient(pSessions),
		FedClient: federation,
		KeyRing:   keyRing,

		AppserviceAPI: asQuery,
		FederationAPI: fedSenderAPI,
		RoomserverAPI: rsAPI,
		UserAPI:       userAPI,
		//ServerKeyAPI:        serverKeyAPI,
		ExtPublicRoomsProvider: rooms.NewPineconeRoomProvider(pRouter, pSessions, fedSenderAPI, federation),
	}
	monolith.AddAllPublicRoutes(processCtx, cfg, routers, cm, &natsInstance, caches, caching.EnableMetrics)

	httpRouter := mux.NewRouter().SkipClean(true).UseEncodedPath()
	httpRouter.PathPrefix(httputil.PublicClientPathPrefix).Handler(routers.Client)
	httpRouter.PathPrefix(httputil.PublicMediaPathPrefix).Handler(routers.Media)

	p2pRouter := pSessions.Protocol("matrix").HTTP().Mux()
	p2pRouter.Handle(httputil.PublicFederationPathPrefix, routers.Federation)
	p2pRouter.Handle(httputil.PublicMediaPathPrefix, routers.Media)

	// Expose the matrix APIs via fetch - for local traffic
	go func() {
		logrus.Info("Listening for service-worker fetch traffic")
		s := JSServer{
			Mux: httpRouter,
		}
		s.ListenAndServe("fetch")
	}()
}
