// SPDX-FileCopyrightText: 2020 Alvar Penning
// SPDX-FileCopyrightText: 2023 Markus Sommer
//
// SPDX-License-Identifier: GPL-3.0-or-later

package application_agent

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"

	"github.com/dtn7/dtn7-go/pkg/bpv7"
	"github.com/dtn7/dtn7-go/pkg/store"
)

// RestAgent is a RESTful Application Agent for simple bundle dispatching.
//
// A client must register itself for some endpoint ID at first. After that, bundles sent to this endpoint can be
// retrieved or new bundles can be sent. For sending, bundles can be created by calling the BundleBuilder. Finally,
// a client should unregister itself.
//
// This is all done by HTTP POSTing JSON objects. Their structure is described in `rest_agent_messages.go` by the types
// with the `Rest` prefix in their names.
//
// A possible conversation follows as an example.
//
//	// 1. Registration of our client, POST to /register
//	// -> {"endpoint_id":"dtn://foo/bar"}
//	// <- {"error":"","uuid":"75be76e2-23fc-da0e-eeb8-4773f84a9d2f"}
//
//	// 2. Fetching bundles for our client, POST to /fetch
//	//    There will be to answers, one with new bundles and one without
//	// -> {"uuid":"75be76e2-23fc-da0e-eeb8-4773f84a9d2f"}
//	// <- {"error":"","bundles":[
//	//      {
//	//        "primaryBlock": {
//	//          "bundleControlFlags":null,
//	//          "destination":"dtn://foo/bar",
//	//          "source":"dtn://sender/",
//	//          "reportTo":"dtn://sender/",
//	//          "creationTimestamp":{"date":"2020-04-14 14:32:06","sequenceNo":0},
//	//          "lifetime":86400000000
//	//        },
//	//        "canonicalBlocks": [
//	//          {"blockNumber":1,"blockTypeCode":1,"blockControlFlags":null,"data":"S2hlbGxvIHdvcmxk"}
//	//        ]
//	//      }
//	//    ]}
//	// <- {"error":"","bundles":[]}
//
//	// 3. Create and dispatch a new bundle, POST to /build
//	// -> {
//	//      "uuid": "75be76e2-23fc-da0e-eeb8-4773f84a9d2f",
//	//      "arguments": {
//	//        "destination": "dtn://dst/",
//	//        "source": "dtn://foo/bar",
//	//        "creation_timestamp_now": 1,
//	//        "lifetime": "24h",
//	//        "payload_block": "hello world"
//	//      }
//	//    }
//	// <- {"error":""}
//
//	// 4. Unregister the client, POST to /unregister
//	// -> {"uuid":"75be76e2-23fc-da0e-eeb8-4773f84a9d2f"}
//	// <- {"error":""}
type RestAgent struct {
	router *mux.Router

	// map UUIDs to EIDs and received bundles
	clients      sync.Map // uuid[string] -> bpv7.EndpointID
	mailboxes    map[string]map[bpv7.BundleID]bpv7.Bundle
	mailboxMutex sync.Mutex
}

// NewRestAgent creates a new RESTful Application Agent.
func NewRestAgent(router *mux.Router) (ra *RestAgent) {
	ra = &RestAgent{
		router:    router,
		mailboxes: make(map[string]map[bpv7.BundleID]bpv7.Bundle),
	}

	ra.router.HandleFunc("/register", ra.handleRegister).Methods(http.MethodPost)
	ra.router.HandleFunc("/unregister", ra.handleUnregister).Methods(http.MethodPost)
	ra.router.HandleFunc("/fetch", ra.handleFetch).Methods(http.MethodPost)
	ra.router.HandleFunc("/build", ra.handleBuild).Methods(http.MethodPost)

	return ra
}

// Deliver checks incoming BundleMessages and puts them inbox.
func (ra *RestAgent) Deliver(bundleDescriptor *store.BundleDescriptor) error {
	var uuids []string
	ra.clients.Range(func(k, v interface{}) bool {
		if bundleDescriptor.Destination == v.(bpv7.EndpointID) {
			uuids = append(uuids, k.(string))
		}
		return false // multiple clients might be registered for some endpoint
	})

	ra.mailboxMutex.Lock()
	bndl, err := bundleDescriptor.Load()
	if err != nil {
		return err
	}
	for _, uuid := range uuids {
		mailbox, exists := ra.mailboxes[uuid]
		if !exists {
			mailbox = map[bpv7.BundleID]bpv7.Bundle{bundleDescriptor.ID: bndl}
			ra.mailboxes[uuid] = mailbox
			log.WithFields(log.Fields{
				"bundle": bundleDescriptor.ID.String(),
				"uuid":   uuid,
			}).Debug("REST Application Agent delivering message to a client's inbox")
		} else {
			_, exists = mailbox[bundleDescriptor.ID]
			if !exists {
				mailbox[bundleDescriptor.ID] = bndl
				log.WithFields(log.Fields{
					"bundle": bundleDescriptor.ID.String(),
					"uuid":   uuid,
				}).Debug("REST Application Agent delivering message to a client's inbox")
			} else {
				log.WithFields(log.Fields{
					"bundle": bundleDescriptor.ID.String(),
					"uuid":   uuid,
				}).Debug("REST Application Agent not delivering message to a client's inbox. Message already present.")
			}
		}
	}
	ra.mailboxMutex.Unlock()

	return nil
}

// randomUuid to be used for authentication. UUID not compliant with RFC 4122.
func (_ *RestAgent) randomUuid() (uuid string, err error) {
	uuidBytes := make([]byte, 16)
	if _, err = rand.Read(uuidBytes); err == nil {
		uuid = fmt.Sprintf("%x-%x-%x-%x-%x",
			uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])
	}
	return
}

// handleRegister processes /register POST requests.
func (ra *RestAgent) handleRegister(w http.ResponseWriter, r *http.Request) {
	var (
		registerRequest  RestRegisterRequest
		registerResponse RestRegisterResponse
	)

	if jsonErr := json.NewDecoder(r.Body).Decode(&registerRequest); jsonErr != nil {
		registerResponse.Error = jsonErr.Error()
	} else if eid, eidErr := bpv7.NewEndpointID(registerRequest.EndpointId); eidErr != nil {
		registerResponse.Error = eidErr.Error()
	} else if uuid, uuidErr := ra.randomUuid(); uuidErr != nil {
		registerResponse.Error = uuidErr.Error()
	} else {
		ra.clients.Store(uuid, eid)
		registerResponse.UUID = uuid
	}

	log.WithFields(log.Fields{
		"request":  registerRequest,
		"response": registerResponse,
	}).Info("Processing REST registration")

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(registerResponse); err != nil {
		log.WithError(err).Warn("Failed to write REST registration response")
	}
}

// handleUnregister processes /unregister POST requests.
func (ra *RestAgent) handleUnregister(w http.ResponseWriter, r *http.Request) {
	var (
		unregisterRequest  RestUnregisterRequest
		unregisterResponse RestUnregisterResponse
	)

	if jsonErr := json.NewDecoder(r.Body).Decode(&unregisterRequest); jsonErr != nil {
		log.WithError(jsonErr).Warn("Failed to parse REST unregistration request")
	} else {
		log.WithField("uuid", unregisterRequest.UUID).Info("Unregister REST client")
		ra.clients.Delete(unregisterRequest.UUID)

		ra.mailboxMutex.Lock()
		delete(ra.mailboxes, unregisterRequest.UUID)
		ra.mailboxMutex.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(unregisterResponse); err != nil {
		log.WithError(err).Warn("Failed to write REST unregistration response")
	}
}

// handleFetch returns the bundles from some client's inbox, called by /fetch.
func (ra *RestAgent) handleFetch(w http.ResponseWriter, r *http.Request) {
	var (
		fetchRequest  RestFetchRequest
		fetchResponse RestFetchResponse
	)

	if jsonErr := json.NewDecoder(r.Body).Decode(&fetchRequest); jsonErr != nil {
		log.WithError(jsonErr).Warn("Failed to parse REST fetch request")
		fetchResponse.Error = jsonErr.Error()
	} else if mailbox, ok := ra.mailboxes[fetchRequest.UUID]; ok {
		log.WithField("uuid", fetchRequest.UUID).Info("REST client fetches bundles")

		ra.mailboxMutex.Lock()
		bundles := make([]bpv7.Bundle, 0, len(mailbox))
		for _, bundle := range mailbox {
			bundles = append(bundles, bundle)
		}

		fetchResponse.Bundles = bundles

		delete(ra.mailboxes, fetchRequest.UUID)
		ra.mailboxMutex.Unlock()
	} else if !ok {
		log.WithField("uuid", fetchRequest.UUID).Debug("REST client has no new bundles to fetch")
		fetchResponse.Bundles = make([]bpv7.Bundle, 0)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(fetchResponse); err != nil {
		log.WithError(err).Warn("Failed to write REST fetch response")
	}
}

// handleBuild creates and dispatches a new bundle, called by /build.
func (ra *RestAgent) handleBuild(w http.ResponseWriter, r *http.Request) {
	var (
		buildRequest  RestBuildRequest
		buildResponse RestBuildResponse
	)

	if jsonErr := json.NewDecoder(r.Body).Decode(&buildRequest); jsonErr != nil {
		log.WithError(jsonErr).Warn("Failed to parse REST build request")
		buildResponse.Error = jsonErr.Error()
	} else if eid, ok := ra.clients.Load(buildRequest.UUID); !ok {
		log.WithField("uuid", buildRequest.UUID).Debug("REST client cannot build for unknown UUID")
		buildResponse.Error = "Invalid UUID"
	} else if b, bErr := bpv7.BuildFromMap(buildRequest.Args); bErr != nil {
		log.WithError(bErr).WithField("uuid", buildRequest.UUID).Warn("REST client failed to build a bundle")
		buildResponse.Error = bErr.Error()
	} else if pb := b.PrimaryBlock; pb.SourceNode != eid && pb.ReportTo != eid {
		msg := "REST client's endpoint is neither the source nor the report_to field"
		log.WithFields(log.Fields{
			"uuid":     buildRequest.UUID,
			"endpoint": eid,
			"bundle":   b.ID().String(),
		}).Warn(msg)
		buildResponse.Error = msg
	} else {
		log.WithFields(log.Fields{
			"uuid":   buildRequest.UUID,
			"bundle": b.ID().String(),
		}).Info("REST client sent bundle")
		GetManagerSingleton().Send(&b)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(buildResponse); err != nil {
		log.WithError(err).Warn("Failed to write REST build response")
	}
}

func (ra *RestAgent) Endpoints() (eids []bpv7.EndpointID) {
	ra.clients.Range(func(_, v interface{}) bool {
		eids = append(eids, v.(bpv7.EndpointID))
		return false
	})
	return
}

func (ra *RestAgent) Shutdown() {

}
