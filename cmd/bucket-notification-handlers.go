/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/gorilla/mux"
)

const (
	bucketConfigPrefix       = "buckets"
	bucketNotificationConfig = "notification.xml"
	bucketListenerConfig     = "listener.json"
)

// GetBucketNotificationHandler - This implementation of the GET
// operation uses the notification subresource to return the
// notification configuration of a bucket. If notifications are
// not enabled on the bucket, the operation returns an empty
// NotificationConfiguration element.
func (api objectAPIHandlers) GetBucketNotificationHandler(w http.ResponseWriter, r *http.Request) {
	objAPI := api.ObjectAPI()
	if objAPI == nil {
		writeErrorResponse(w, r, ErrServerNotInitialized, r.URL.Path)
		return
	}

	// Validate request authorization.
	if s3Error := checkAuth(r); s3Error != ErrNone {
		writeErrorResponse(w, r, s3Error, r.URL.Path)
		return
	}
	vars := mux.Vars(r)
	bucket := vars["bucket"]
	// Attempt to successfully load notification config.
	nConfig, err := loadNotificationConfig(bucket, objAPI)
	if err != nil && err != errNoSuchNotifications {
		errorIf(err, "Unable to read notification configuration.")
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}
	// For no notifications we write a dummy XML.
	if err == errNoSuchNotifications {
		// Complies with the s3 behavior in this regard.
		nConfig = &notificationConfig{}
	}
	notificationBytes, err := xml.Marshal(nConfig)
	if err != nil {
		// For any marshalling failure.
		errorIf(err, "Unable to marshal notification configuration into XML.", err)
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}
	// Success.
	writeSuccessResponse(w, notificationBytes)
}

// PutBucketNotificationHandler - Minio notification feature enables
// you to receive notifications when certain events happen in your bucket.
// Using this API, you can replace an existing notification configuration.
// The configuration is an XML file that defines the event types that you
// want Minio to publish and the destination where you want Minio to publish
// an event notification when it detects an event of the specified type.
// By default, your bucket has no event notifications configured. That is,
// the notification configuration will be an empty NotificationConfiguration.
func (api objectAPIHandlers) PutBucketNotificationHandler(w http.ResponseWriter, r *http.Request) {
	objectAPI := api.ObjectAPI()
	if objectAPI == nil {
		writeErrorResponse(w, r, ErrServerNotInitialized, r.URL.Path)
		return
	}

	// Validate request authorization.
	if s3Error := checkAuth(r); s3Error != ErrNone {
		writeErrorResponse(w, r, s3Error, r.URL.Path)
		return
	}
	vars := mux.Vars(r)
	bucket := vars["bucket"]

	_, err := objectAPI.GetBucketInfo(bucket)
	if err != nil {
		errorIf(err, "Unable to find bucket info.")
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}

	// If Content-Length is unknown or zero, deny the request. PutBucketNotification
	// always needs a Content-Length if incoming request is not chunked.
	if !contains(r.TransferEncoding, "chunked") {
		if r.ContentLength == -1 {
			writeErrorResponse(w, r, ErrMissingContentLength, r.URL.Path)
			return
		}
	}

	// Reads the incoming notification configuration.
	var buffer bytes.Buffer
	if r.ContentLength >= 0 {
		_, err = io.CopyN(&buffer, r.Body, r.ContentLength)
	} else {
		_, err = io.Copy(&buffer, r.Body)
	}
	if err != nil {
		errorIf(err, "Unable to read incoming body.")
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}

	var notificationCfg notificationConfig
	// Unmarshal notification bytes.
	notificationConfigBytes := buffer.Bytes()
	if err = xml.Unmarshal(notificationConfigBytes, &notificationCfg); err != nil {
		errorIf(err, "Unable to parse notification configuration XML.")
		writeErrorResponse(w, r, ErrMalformedXML, r.URL.Path)
		return
	} // Successfully marshalled notification configuration.

	// Validate unmarshalled bucket notification configuration.
	if s3Error := validateNotificationConfig(notificationCfg); s3Error != ErrNone {
		writeErrorResponse(w, r, s3Error, r.URL.Path)
		return
	}

	// Put bucket notification config.
	err = PutBucketNotificationConfig(bucket, &notificationCfg, objectAPI)
	if err != nil {
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}

	// Success.
	writeSuccessResponse(w, nil)
}

// PutBucketNotificationConfig - Put a new notification config for a
// bucket (overwrites any previous config) persistently, updates
// global in-memory state, and notify other nodes in the cluster (if
// any)
func PutBucketNotificationConfig(bucket string, ncfg *notificationConfig, objAPI ObjectLayer) error {
	if ncfg == nil {
		return errInvalidArgument
	}

	// Acquire a write lock on bucket before modifying its
	// configuration.
	opsID := getOpsID()
	nsMutex.Lock(bucket, "", opsID)
	// Release lock after notifying peers
	defer nsMutex.Unlock(bucket, "", opsID)

	// persist config to disk
	err := persistNotificationConfig(bucket, ncfg, objAPI)
	if err != nil {
		return fmt.Errorf("Unable to persist Bucket notification config to object layer - config=%v errMsg=%v", *ncfg, err)
	}

	// All servers (including local) are told to update in-memory
	// config
	S3PeersUpdateBucketNotification(bucket, ncfg)

	return nil
}

// writeNotification marshals notification message before writing to client.
func writeNotification(w http.ResponseWriter, notification map[string][]NotificationEvent) error {
	// Invalid response writer.
	if w == nil {
		return errInvalidArgument
	}
	// Invalid notification input.
	if notification == nil {
		return errInvalidArgument
	}
	// Marshal notification data into JSON and write to client.
	notificationBytes, err := json.Marshal(&notification)
	if err != nil {
		return err
	}
	// Add additional CRLF characters for client to
	// differentiate the individual events properly.
	_, err = w.Write(append(notificationBytes, crlf...))
	// Make sure we have flushed, this would set Transfer-Encoding: chunked.
	w.(http.Flusher).Flush()
	if err != nil {
		return err
	}
	return nil
}

// CRLF character used for chunked transfer in accordance with HTTP standards.
var crlf = []byte("\r\n")

// sendBucketNotification - writes notification back to client on the response writer
// for each notification input, otherwise writes whitespace characters periodically
// to keep the connection active. Each notification messages are terminated by CRLF
// character. Upon any error received on response writer the for loop exits.
func sendBucketNotification(w http.ResponseWriter, arnListenerCh <-chan []NotificationEvent) {
	var dummyEvents = map[string][]NotificationEvent{"Records": nil}
	// Continuously write to client either timely empty structures
	// every 5 seconds, or return back the notifications.
	for {
		select {
		case events := <-arnListenerCh:
			if err := writeNotification(w, map[string][]NotificationEvent{"Records": events}); err != nil {
				errorIf(err, "Unable to write notification to client.")
				return
			}
		case <-time.After(globalSNSConnAlive): // Wait for global conn active seconds.
			if err := writeNotification(w, dummyEvents); err != nil {
				// FIXME - do not log for all errors.
				errorIf(err, "Unable to write notification to client.")
				return
			}
		}
	}
}

// ListenBucketNotificationHandler - list bucket notifications.
func (api objectAPIHandlers) ListenBucketNotificationHandler(w http.ResponseWriter, r *http.Request) {
	// Validate if bucket exists.
	objAPI := api.ObjectAPI()
	if objAPI == nil {
		writeErrorResponse(w, r, ErrServerNotInitialized, r.URL.Path)
		return
	}

	// Validate request authorization.
	if s3Error := checkAuth(r); s3Error != ErrNone {
		writeErrorResponse(w, r, s3Error, r.URL.Path)
		return
	}
	vars := mux.Vars(r)
	bucket := vars["bucket"]

	// Parse listen bucket notification resources.
	prefixes, suffixes, events := getListenBucketNotificationResources(r.URL.Query())

	if err := validateFilterValues(prefixes); err != ErrNone {
		writeErrorResponse(w, r, err, r.URL.Path)
		return
	}

	if err := validateFilterValues(suffixes); err != ErrNone {
		writeErrorResponse(w, r, err, r.URL.Path)
		return
	}

	// Validate all the resource events.
	for _, event := range events {
		if errCode := checkEvent(event); errCode != ErrNone {
			writeErrorResponse(w, r, errCode, r.URL.Path)
			return
		}
	}

	_, err := objAPI.GetBucketInfo(bucket)
	if err != nil {
		errorIf(err, "Unable to get bucket info.")
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}

	accountID := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	accountARN := fmt.Sprintf(
		"%s:%s:%s:%s-%s",
		minioTopic,
		serverConfig.GetRegion(),
		accountID,
		snsTypeMinio,
		globalMinioAddr,
	)
	var filterRules []filterRule

	for _, prefix := range prefixes {
		filterRules = append(filterRules, filterRule{
			Name:  "prefix",
			Value: prefix,
		})
	}

	for _, suffix := range suffixes {
		filterRules = append(filterRules, filterRule{
			Name:  "suffix",
			Value: suffix,
		})
	}

	// Make topic configuration corresponding to this
	// ListenBucketNotification request.
	topicCfg := &topicConfig{
		TopicARN: accountARN,
		ServiceConfig: ServiceConfig{
			Events: events,
			Filter: struct {
				Key keyFilter `xml:"S3Key,omitempty" json:"S3Key,omitempty"`
			}{
				Key: keyFilter{
					FilterRules: filterRules,
				},
			},
			ID: "sns-" + accountID,
		},
	}

	// Setup a listening channel that will receive notifications
	// from the RPC handler.
	nEventCh := make(chan []NotificationEvent)
	defer close(nEventCh)
	// Add channel for listener events
	if err = globalEventNotifier.AddListenerChan(accountARN, nEventCh); err != nil {
		errorIf(err, "Error adding a listener!")
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}
	// Remove listener channel after the writer has closed or the
	// client disconnected.
	defer globalEventNotifier.RemoveListenerChan(accountARN)

	// Update topic config to bucket config and persist - as soon
	// as this call compelets, events may start appearing in
	// nEventCh
	lc := listenerConfig{
		TopicConfig:  *topicCfg,
		TargetServer: globalMinioAddr,
	}

	err = AddBucketListenerConfig(bucket, &lc, objAPI)
	if err != nil {
		writeErrorResponse(w, r, toAPIErrorCode(err), r.URL.Path)
		return
	}
	defer RemoveBucketListenerConfig(bucket, &lc, objAPI)

	// Add all common headers.
	setCommonHeaders(w)

	// Start sending bucket notifications.
	sendBucketNotification(w, nEventCh)
}

// AddBucketListenerConfig - Updates on disk state of listeners, and
// updates all peers with the change in listener config.
func AddBucketListenerConfig(bucket string, lcfg *listenerConfig, objAPI ObjectLayer) error {
	if lcfg == nil {
		return errInvalidArgument
	}
	listenerCfgs := globalEventNotifier.GetBucketListenerConfig(bucket)

	// add new lid to listeners and persist to object layer.
	listenerCfgs = append(listenerCfgs, *lcfg)

	// Acquire a write lock on bucket before modifying its
	// configuration.
	opsID := getOpsID()
	nsMutex.Lock(bucket, "", opsID)
	// Release lock after notifying peers
	defer nsMutex.Unlock(bucket, "", opsID)

	// update persistent config if dist XL
	if globalIsDistXL {
		err := persistListenerConfig(bucket, listenerCfgs, objAPI)
		if err != nil {
			errorIf(err, "Error persisting listener config when adding a listener.")
			return err
		}
	}

	// persistence success - now update in-memory globals on all
	// peers (including local)
	S3PeersUpdateBucketListener(bucket, listenerCfgs)
	return nil
}

// RemoveBucketListenerConfig - removes a given bucket notification config
func RemoveBucketListenerConfig(bucket string, lcfg *listenerConfig, objAPI ObjectLayer) {
	listenerCfgs := globalEventNotifier.GetBucketListenerConfig(bucket)

	// remove listener with matching ARN - if not found ignore and
	// exit.
	var updatedLcfgs []listenerConfig
	found := false
	for k, configuredLcfg := range listenerCfgs {
		if configuredLcfg.TopicConfig.TopicARN == lcfg.TopicConfig.TopicARN {
			updatedLcfgs = append(listenerCfgs[:k],
				listenerCfgs[k+1:]...)
			found = true
			break
		}
	}
	if !found {
		return
	}

	// Acquire a write lock on bucket before modifying its
	// configuration.
	opsID := getOpsID()
	nsMutex.Lock(bucket, "", opsID)
	// Release lock after notifying peers
	defer nsMutex.Unlock(bucket, "", opsID)

	// update persistent config if dist XL
	if globalIsDistXL {
		err := persistListenerConfig(bucket, updatedLcfgs, objAPI)
		if err != nil {
			errorIf(err, "Error persisting listener config when removing a listener.")
			return
		}
	}

	// persistence success - now update in-memory globals on all
	// peers (including local)
	S3PeersUpdateBucketListener(bucket, updatedLcfgs)
}

// Removes notification.xml for a given bucket, only used during DeleteBucket.
func removeNotificationConfig(bucket string, objAPI ObjectLayer) error {
	// Verify bucket is valid.
	if !IsValidBucketName(bucket) {
		return BucketNameInvalid{Bucket: bucket}
	}

	notificationConfigPath := path.Join(bucketConfigPrefix, bucket, bucketNotificationConfig)
	return objAPI.DeleteObject(minioMetaBucket, notificationConfigPath)
}
