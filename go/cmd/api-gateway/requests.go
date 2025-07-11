// This file contains the handlers for the requests that the API Gateway receives from the client
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Jorrit05/DYNAMOS/pkg/api"
	"github.com/Jorrit05/DYNAMOS/pkg/lib"
	pb "github.com/Jorrit05/DYNAMOS/pkg/proto"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.opencensus.io/trace"
)

func requestHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("Starting requestApprovalHandler")
		// Requests may take up to 10 minutes now
		ctxWithTimeout, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		// Start a new span with the context that has a timeout
		ctx, span := trace.StartSpan(ctxWithTimeout, "requestApprovalHandler")
		defer span.End()

		body, err := api.GetRequestBody(w, r, serviceName)
		if err != nil {
			return
		}

		var apiReqApproval api.RequestApproval
		if err := json.Unmarshal(body, &apiReqApproval); err != nil {
			logger.Sugar().Errorf("Error unmMarshalling get apiReqApproval: %v", err)
			return
		}

		userPb := &pb.User{
			Id:       apiReqApproval.User.Id,
			UserName: apiReqApproval.User.UserName,
		}

		var dataRequestInterface map[string]any
		if err := json.Unmarshal(apiReqApproval.DataRequest, &dataRequestInterface); err != nil {
			logger.Sugar().Errorf("Error unmarhsalling get request: %v", err)
			return
		}

		dataRequestOptions := &api.DataRequestOptions{}
		dataRequestOptions.Options = make(map[string]bool)
		if err := json.Unmarshal(apiReqApproval.DataRequest, &dataRequestOptions); err != nil {
			logger.Sugar().Errorf("Error unmMarshalling get apiReqApproval: %v", err)
			return
		}

		dataRequestInterface["user"] = userPb

		// Create protobuf struct for the req approval flow
		protoRequest := &pb.RequestApproval{
			Type:             apiReqApproval.Type,
			User:             userPb,
			DataProviders:    apiReqApproval.DataProviders,
			DestinationQueue: "policyEnforcer-in",
			Options:          dataRequestOptions.Options,
		}

		// Create a channel to receive the response
		responseChan := make(chan validation)

		requestApprovalMutex.Lock()
		requestApprovalMap[protoRequest.User.Id] = responseChan
		requestApprovalMutex.Unlock()

		_, err = c.SendRequestApproval(ctx, protoRequest)
		if err != nil {
			logger.Sugar().Errorf("error in sending requestapproval: %v", err)
		}

		select {
		case validationStruct := <-responseChan:
			msg := validationStruct.response

			logger.Sugar().Infof("Received response, %s", msg.Type)
			if msg.Type != "requestApprovalResponse" {
				logger.Sugar().Errorf("Unexpected message received, type: %s", msg.Type)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}

			requestMetadata := &pb.RequestMetadata{
				JobId: msg.JobId,
			}
			dataRequestInterface["requestMetadata"] = requestMetadata

			logger.Sugar().Infof("Data Prepared jsonData: %s", dataRequestInterface)

			var response []byte

			if apiReqApproval.Type == "vflTrainModelRequest" {
				ctxWithoutCancel := context.WithoutCancel(r.Context())
				response = runVFLTraining(dataRequestInterface, msg.AuthorizedProviders, msg.JobId, ctxWithoutCancel)
			} else {
				// Marshal the combined data back into JSON for forwarding
				dataRequestJson, err := json.Marshal(dataRequestInterface)
				if err != nil {
					logger.Sugar().Errorf("Error marshalling combined data: %v", err)
					return
				}

				response = sendDataToAuthProviders(dataRequestJson, msg.AuthorizedProviders, apiReqApproval.Type, msg.JobId)
			}

			w.WriteHeader(http.StatusOK)
			w.Write(response)
			return

		case <-ctx.Done():
			http.Error(w, "Request timed out", http.StatusRequestTimeout)
			return
		}
	}
}

func runVFLTrainingRound(dataRequest map[string]any, clients map[string]string, serverAuth string, serverUrl string, learning_rate float64) (float64, error) {
	var wg sync.WaitGroup
	responses := map[string]string{}

	for auth, url := range clients {
		wg.Add(1)
		target := strings.ToLower(auth)

		ips, err := net.LookupIP(url)
		if err == nil && len(ips) != 0 {
			url = ips[0].String()
		}

		endpoint := fmt.Sprintf("http://%s:8080/agent/v1/vflTrainRequest/%s", url, target)

		dataRequest["type"] = "vflTrainRequest"

		dataRequestJson, err := json.Marshal(dataRequest)
		if err != nil {
			logger.Sugar().Errorf("Error marshalling combined data: %v", err)
			return 0., err
		}

		go func() {
			responseData, err := sendData(endpoint, dataRequestJson)

			if err != nil {
				logger.Sugar().Errorf("Error sending data, %v", err)
			} else {
				responseJson := &pb.MicroserviceCommunication{}
				err = json.Unmarshal([]byte(responseData), responseJson)

				if err != nil {
					logger.Sugar().Error("Unmarshalling response did not go well: ", err)
				}

				dataJson := responseJson.Data.AsMap()
				embeddings, ok := dataJson["embeddings"].(string)

				if !ok {
					logger.Sugar().Error("No embeddings found in the return data.")
					embeddings = ""
					// TODO: Handle disagreements?
				}

				responses[target] = embeddings
			}

			wg.Done()
		}()
	}

	wg.Wait()

	target := strings.ToLower(serverAuth)
	endpoint := fmt.Sprintf("http://%s:8080/agent/v1/vflTrainRequest/%s", serverUrl, target)

	dataRequest["type"] = "vflAggregateRequest"
	dataRequest["data"] = map[string]any{
		"embeddings": []string{responses["clientone"], responses["clienttwo"], responses["clientthree"]},
	}

	dataRequestJson, err := json.Marshal(dataRequest)
	if err != nil {
		logger.Sugar().Errorf("Error marshalling combined data: %v", err)
		return 0., err
	}

	responseData, error := sendData(endpoint, dataRequestJson)
	if error != nil {
		logger.Sugar().Errorf("Error sending data to the server, %v", error)
	}

	serverResponse := &pb.MicroserviceCommunication{}
	err = json.Unmarshal([]byte(responseData), serverResponse)

	if err != nil {
		logger.Sugar().Error("Unmarshalling response did not go well: ", err)
	}

	accuracy := serverResponse.Data.GetFields()["accuracy"].GetNumberValue()
	gradientList := serverResponse.Data.GetFields()["gradients"].GetListValue().GetValues()

	gradients := []string{}
	for _, val := range gradientList {
		gradients = append(gradients, val.GetStringValue())
	}

	// TODO: Send the gradients back to the client to update their models
	index := 0
	for auth, url := range clients {
		wg.Add(1)
		target := strings.ToLower(auth)
		endpoint := fmt.Sprintf("http://%s:8080/agent/v1/vflTrainRequest/%s", url, target)

		dataRequest["type"] = "vflGradientDescentRequest"
		dataRequest["data"] = map[string]any{
			"gradients":     gradients[index],
			"learning_rate": learning_rate,
		}

		index++

		dataRequestJson, err := json.Marshal(dataRequest)
		if err != nil {
			logger.Sugar().Errorf("Error marshalling combined data: %v", err)
			return 0., err
		}

		go func() {
			response, err := sendData(endpoint, dataRequestJson)
			if err != nil {
				logger.Sugar().Error("Error sending data, ", err, ", received: ", response)
			}
			wg.Done()
		}()
	}

	wg.Wait()

	return accuracy, nil
}

func runVFLTraining(dataRequest map[string]any, authorizedProviders map[string]string, jobId string, ctx context.Context) []byte {
	clients := map[string]string{}
	var serverUrl string
	var serverAuth string
	var finalAccuracy float64
	var wg sync.WaitGroup

	var cycles int64 = 10
	var learning_rate float64 = 0.05
	var change_policies int64 = -1
	var dataProviders []string = []string{}

	data, ok := dataRequest["data"].(map[string]any)
	logger.Sugar().Info("Data from req: ", data)

	if ok {
		floatCycles, ok := data["cycles"].(float64)

		if ok {
			cycles = int64(floatCycles)
		}

		floatLearningRate, ok := data["learning_rate"].(float64)
		if ok {
			learning_rate = floatLearningRate
		}

		changePolicies, ok := data["change_policies"].(float64)
		if ok {
			change_policies = int64(changePolicies)
		}
	}

	for auth, url := range authorizedProviders {
		if strings.ToLower(auth) == "server" {
			serverUrl = url
			serverAuth = auth
		} else if url != "" {
			clients[auth] = url
		}

		dataProviders = append(dataProviders, auth)
	}

	logger.Sugar().Info("Sending ping to start pods...")
	dataRequest["type"] = "vflPingRequest"

	dataRequestJson, err := json.Marshal(dataRequest)
	if err != nil {
		logger.Sugar().Errorf("Error marshalling combined data: %v", err)
		return []byte{}
	}

	user, ok := dataRequest["user"].(*pb.User)

	if !ok {
		logger.Sugar().Info("Did not retrieve User from dataRequest, cannot dynamically verify each training round.")
		user = &pb.User{}
	}

	var noPing bool = false

	for auth, url := range authorizedProviders {
		wg.Add(1)
		target := strings.ToLower(auth)
		endpoint := fmt.Sprintf("http://%s:8080/agent/v1/vflTrainRequest/%s", url, target)

		go func() {
			// TODO: Repeat ping until no error, after 5 tries, cancel request
			for i := range 5 {
				_, err := sendData(endpoint, dataRequestJson)

				if err == nil {
					break
				}

				if i == 4 {
					noPing = true
				}
			}

			wg.Done()
		}()
	}

	if noPing {
		logger.Sugar().Error("No ping from a client or the server. Something is wrong.")
	}

	wg.Wait()

	logger.Sugar().Info("Running VFL for ", cycles, " rounds")
	for round := range cycles {
		logger.Sugar().Info("Running VFL training round ", round)

		// TODO: Implement policy change request
		if change_policies == round {
			logger.Sugar().Info("Sending in the policy change request, removing client 3 from the agreement.")
			logger.Sugar().Info("TODO: Policy change request not yet implemented.")

			policyUpdate := &pb.RequestApproval{
				Type:             "policyRemoval",
				User:             user,
				DestinationQueue: "policyEnforcer-in",
			}

			// Create a channel to receive the response
			responseChan := make(chan validation)

			requestApprovalMutex.Lock()
			requestApprovalMap[policyUpdate.User.Id] = responseChan
			requestApprovalMutex.Unlock()

			logger.Sugar().Info("- Sending policy removal request")
			_, err = c.SendRequestApproval(ctx, policyUpdate)
			if err != nil {
				logger.Sugar().Warnf("error in sending/receiving policy removal: %v", err)
			}
		}

		protoRequest := &pb.RequestApproval{
			Type:             "vflTrainModelRequest",
			User:             user,
			DataProviders:    dataProviders,
			DestinationQueue: "policyEnforcer-in",
		}

		// Create a channel to receive the response
		responseChan := make(chan validation)

		requestApprovalMutex.Lock()
		requestApprovalMap[protoRequest.User.Id] = responseChan
		requestApprovalMutex.Unlock()

		noValidation := false

		logger.Sugar().Info("- Sending policy reverification request")
		for i := range 5 {
			_, err = c.SendRequestApproval(ctx, protoRequest)
			if err != nil {
				logger.Sugar().Warnf("error in sending/receiving requestApproval: %v", err)
			}

			if err == nil {
				break
			}

			if i == 4 {
				noValidation = true
			}
		}

		if noValidation {
			logger.Sugar().Error("No reverification approval received, error in network. Shutting down operation.")
			break
		}

		select {
		case validationStruct := <-responseChan:
			msg := validationStruct.response
			logger.Sugar().Info("Received validation message: ", msg, ", with vstruct: ", validationStruct)

			if msg.Type != "requestApprovalResponse" {
				logger.Sugar().Errorf("Unexpected message received, type: %s", msg.Type)
				return []byte{}
			}

			if msg.Error != "" || len(msg.AuthorizedProviders) != len(authorizedProviders) {
				logger.Sugar().Info("-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-")
				logger.Sugar().Info("   Policy does not allow this training to continue.")
				logger.Sugar().Info("-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-")
				break
			}

			logger.Sugar().Info("- Sending training request")
			accuracy, err := runVFLTrainingRound(dataRequest, clients, serverAuth, serverUrl, learning_rate)
			logger.Sugar().Info("- Intermediate accuracy achieved: ", accuracy, " for round ", round)
			finalAccuracy = accuracy

			if err != nil {
				logger.Sugar().Error("Training round returned an error.")
				break
			}
			// default:
		}
	}

	logger.Sugar().Info("-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-")
	logger.Sugar().Info("Final accuracy achieved: ", finalAccuracy)
	logger.Sugar().Info("-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-")

	dataRequest["type"] = "vflShutdownRequest"

	dataRequestJson, err = json.Marshal(dataRequest)
	if err != nil {
		logger.Sugar().Errorf("Error marshalling combined data: %v", err)
		return []byte{}
	}

	for auth, url := range authorizedProviders {
		wg.Add(1)
		target := strings.ToLower(auth)
		endpoint := fmt.Sprintf("http://%s:8080/agent/v1/vflTrainRequest/%s", url, target)

		go func() {
			sendData(endpoint, dataRequestJson)
			wg.Done()
		}()
	}

	wg.Wait()

	response := map[string]any{
		"jobId":    jobId,
		"accuracy": finalAccuracy,
	}

	logger.Sugar().Info("-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-")
	logger.Sugar().Info("Final accuracy achieved: ", finalAccuracy)
	logger.Sugar().Info("-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-=-")

	return cleanupAndMarshalResponse(response)
}

// Use the data request that was previously built and send it to the authorised providers
// acquired from the request approval
func sendDataToAuthProviders(dataRequest []byte, authorizedProviders map[string]string, msgType string, jobId string) []byte {
	// Setup the wait group for async data requests
	var wg sync.WaitGroup
	var responses []string

	// This will be replaced with AMQ in the future
	agentPort := "8080"
	// Iterate over each auth provider
	for auth, url := range authorizedProviders {
		wg.Add(1)
		target := strings.ToLower(auth)
		// Construct the end point
		endpoint := fmt.Sprintf("http://%s:%s/agent/v1/%s/%s", url, agentPort, msgType, target)

		logger.Sugar().Infof("Sending request to %s.\nEndpoint: %s\nJSON:%v", target, endpoint, string(dataRequest))

		// Async call send the data
		go func() {
			respData, err := sendData(endpoint, dataRequest)
			if err != nil {
				logger.Sugar().Errorf("Error sending data, %v", err)
			}
			responses = append(responses, respData)
			// Signal that the data request has been sent to all auth providers
			wg.Done()
		}()
	}

	// Wait until all the requests are complete
	wg.Wait()
	logger.Sugar().Debug("Returning responses")

	responseMap := map[string]any{
		"jobId":     jobId,
		"responses": responses,
	}

	// jsonResponse, _ := json.Marshal(responseMap)
	// return jsonResponse
	return cleanupAndMarshalResponse(responseMap)
}

// Now assumes input is map[string]interface{} and directly marshals it to prettified JSON.
func cleanupAndMarshalResponse(responseMap map[string]any) []byte {
	prettifiedJSON, err := json.MarshalIndent(responseMap, "", "    ")
	if err != nil {
		logger.Sugar().Errorf("Error marshalling cleaned response: %v", err)
	}
	return prettifiedJSON
}

func sendData(endpoint string, jsonData []byte) (string, error) {
	// FIXME: Change to an actual token in the future?
	headers := map[string]string{
		"Authorization": "bearer 1234",
	}
	body, err := api.PostRequest(endpoint, string(jsonData), headers)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func availableProvidersHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("Starting requestApprovalHandler")
		var availableProviders = make(map[string]lib.AgentDetails)
		resp, err := getAvailableProviders()
		if err != nil {
			logger.Sugar().Errorf("Error getting available providers: %v", err)
			return
		}

		// Bind resp to availableProviders
		availableProviders = resp

		jsonResponse, err := json.Marshal(availableProviders)
		if err != nil {
			logger.Sugar().Errorf("Error marshalling result, %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write(jsonResponse)
	}
}

// Maybe this should be moved into the orchestrarot
func getAvailableProviders() (map[string]lib.AgentDetails, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Get the value from etcd.
	resp, err := etcdClient.Get(ctx, "/agents/online", clientv3.WithPrefix())
	if err != nil {
		logger.Sugar().Errorf("failed to get value from etcd: %v", err)
		return nil, err
	}

	// Initialize an empty map to store the unmarshaled structs.
	result := make(map[string]lib.AgentDetails)
	// Iterate through the key-value pairs and unmarshal the values into structs.
	for _, kv := range resp.Kvs {
		var target lib.AgentDetails
		err = json.Unmarshal(kv.Value, &target)
		if err != nil {
			// return nil, fmt.Errorf("failed to unmarshal JSON for key %s: %v", key, err)
		}
		result[string(target.Name)] = target
	}

	return result, nil

}
