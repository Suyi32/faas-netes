// Copyright (c) Alex Ellis 2017. All rights reserved.
// Copyright 2020 OpenFaaS Author(s)
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package k8s

import (
	"fmt"
	// "math/rand"
	"net/url"
	"net/http"
	"strings"
	"log"
	"sync"
	"encoding/json"
	"bytes"
	"sync/atomic"
	corelister "k8s.io/client-go/listers/core/v1"
)

// watchdogPort for the OpenFaaS function watchdog
const watchdogPort = 8080

func containsIP(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}

type Queue struct {
	mu       sync.Mutex
	capacity int
	q        []string
}

// Insert inserts the item into the queue
func (q *Queue) Insert(item int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.q) < int(q.capacity) {
		q.q = append(q.q, item)
		return nil
	}
	return errors.New("Queue is full")
}

func (q *Queue) Tail() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.q) > 0 {
		item := q.q[ len(q.q) - 1 ]
		return item
	}
	return ""
}

func (q *Queue) RemoveTail() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.q) > 0 {
		q.q = q.q[:len(q.q) - 1]
	}
}

// Remove removes the oldest element from the queue
func (q *Queue) Remove() (int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.q) > 0 {
		item := q.q[0]
		q.q = q.q[1:]
		return item
	}
	return 0
}

// CreateQueue creates an empty queue with desired capacity
func CreateQueue(capacity int) *Queue {
	return &Queue{
		capacity: capacity,
		q:        make([]string, "", capacity),
	}
}

type ScaleServiceRequest struct {
	ServiceName string `json:"serviceName"`
	Replicas    uint64 `json:"replicas"`
}

func QueryContext(endpoint string) map[string]float64 {
	resp, err := http.Get(fmt.Sprintf("http://%s:%d/_/context", endpoint, watchdogPort))
	if err != nil {
		log.Println("Query Context Error")
		panic(err.Error())
	}
	defer resp.Body.Close()

	var respJson map[string]float64
	err = json.NewDecoder(resp.Body).Decode(&respJson)
	if err != nil {
		log.Println("Error while parsing context response.")
		panic(err)
	}
	return respJson
}

func NewFunctionLookup(ns string, lister corelister.EndpointsLister) *FunctionLookup {
	return &FunctionLookup{
		DefaultNamespace: ns,
		EndpointLister:   lister,
		Listers:          map[string]corelister.EndpointsNamespaceLister{},
		lock:             sync.RWMutex{},
		ScaleLocker:      0,
		FuncQueue: 		  map[string]*Queue,
	}
}

type FunctionLookup struct {
	DefaultNamespace string
	EndpointLister   corelister.EndpointsLister
	Listers          map[string]corelister.EndpointsNamespaceLister

	lock sync.RWMutex
	ScaleLocker uint32
	FuncQueue map[string]Queue
}

func (f *FunctionLookup) GetLister(ns string) corelister.EndpointsNamespaceLister {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.Listers[ns]
}

func (f *FunctionLookup) SetLister(ns string, lister corelister.EndpointsNamespaceLister) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.Listers[ns] = lister
}

func getNamespace(name, defaultNamespace string) string {
	namespace := defaultNamespace
	if strings.Contains(name, ".") {
		namespace = name[strings.LastIndexAny(name, ".")+1:]
	}
	return namespace
}

func (l *FunctionLookup) Resolve(name string) (url.URL, string, error) {
	functionName := name
	namespace := getNamespace(name, l.DefaultNamespace)
	if err := l.verifyNamespace(namespace); err != nil {
		return url.URL{}, "", err
	}

	if strings.Contains(name, ".") {
		functionName = strings.TrimSuffix(name, "."+namespace)
	}

	nsEndpointLister := l.GetLister(namespace)

	if nsEndpointLister == nil {
		l.SetLister(namespace, l.EndpointLister.Endpoints(namespace))

		nsEndpointLister = l.GetLister(namespace)
	}

	svc, err := nsEndpointLister.Get(functionName)
	if err != nil {
		return url.URL{}, "", fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
	}

	if len(svc.Subsets) == 0 {
		return url.URL{}, "", fmt.Errorf("no subsets available for \"%s.%s\"", functionName, namespace)
	}

	// all := len(svc.Subsets[0].Addresses)
	if len(svc.Subsets[0].Addresses) == 0 {
		return url.URL{}, "", fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
	}

	goldIPs := []string{}
	for _, v := range svc.Subsets[0].Addresses {
		goldIPs = append(goldIPs, v.IP)
	}

	if _, ok := l.FuncQueue[functionName]; !ok {
		log.Println(functionName, "Queue Not exist. Create one")
		l.FuncQueue[functionName] = CreateQueue(999)
	}

	if len(l.FuncQueue[functionName].q) == 0 {
		serviceIP := svc.Subsets[0].Addresses[0].IP
		l.FuncQueue[functionName].q.Insert(serviceIP)
	} else {
		tailIP := l.FuncQueue[functionName].q.Tail()
		if !containsIP(goldIPs, tailIP) {
			l.FuncQueue[functionName].q.RemoveTail()
		}
		if contextJson := QueryContext(tailIP)
	}

	// target := rand.Intn(all)
	// serviceIP := svc.Subsets[0].Addresses[target].IP
	var serviceIP string = "none"
	for _, v := range svc.Subsets[0].Addresses {
		contextJson := QueryContext(v.IP)
		if (contextJson["MaxConn"] != contextJson["InFlight"]) {
			serviceIP = v.IP
			log.Println(functionName, "chooses", serviceIP)
			break
		}
	}
	if serviceIP == "none" {
		if !atomic.CompareAndSwapUint32(&l.ScaleLocker, 0, 1) {
			return url.URL{}, "", fmt.Errorf("no addresses in subset for \"%s.%s\" now. Will scale later.", functionName, namespace)
		} 
		defer atomic.StoreUint32(&l.ScaleLocker, 0)

		svcLater, err := nsEndpointLister.Get(functionName)
		if len(svcLater.Subsets[0].Addresses) > len(svc.Subsets[0].Addresses)  {
			return url.URL{}, "", fmt.Errorf("no addresses in subset for \"%s.%s\" now. Already scaled.", functionName, namespace)
		}

		provider_url := "http://127.0.0.1:8081/"
		urlPath := fmt.Sprintf("%ssystem/scale-function/%s?namespace=%s", provider_url, functionName, "openfaas-fn")
		scaleReq := ScaleServiceRequest{
			ServiceName: functionName,
			Replicas:    uint64(len(svc.Subsets[0].Addresses) + 1),
		}
		requestBody, err := json.Marshal(scaleReq)
		_, err = http.Post(urlPath, "application/json", bytes.NewBuffer(requestBody))
		if err != nil {
			log.Println("Error while sending Function Scale request.")
			panic(err)
		}
		return url.URL{}, "", fmt.Errorf("no addresses in subset for \"%s.%s\" now. Will scale now.", functionName, namespace)
	}

	urlStr := fmt.Sprintf("http://%s:%d", serviceIP, watchdogPort)

	urlRes, err := url.Parse(urlStr)
	if err != nil {
		return url.URL{}, "", err
	}

	contextString, _ := json.Marshal( QueryContext(serviceIP) )
	return *urlRes, string(contextString), nil
}

func (l *FunctionLookup) verifyNamespace(name string) error {
	if name != "kube-system" {
		return nil
	}
	// ToDo use global namepace parse and validation
	return fmt.Errorf("namespace not allowed")
}
