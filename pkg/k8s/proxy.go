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
	"context"

	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

type ScaleServiceRequest struct {
	ServiceName string `json:"serviceName"`
	Replicas    uint64 `json:"replicas"`
}

func QueryContext(endpoint string) (map[string]float64, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s:%d/_/context", endpoint, watchdogPort))
	if err != nil {
		log.Println("Query Context Error in QueryContext")
		return nil, err
	}
	defer resp.Body.Close()

	var respJson map[string]float64
	err = json.NewDecoder(resp.Body).Decode(&respJson)
	if err != nil {
		log.Println("Error while parsing context response.")
		panic(err)
	}
	return respJson, nil
}

func QueryStatus(endpoint string) (map[string]int64, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s:%d/_/status", endpoint, watchdogPort))
	if err != nil {
		log.Println("Query Statue Error in QueryStatus")
		return nil, err
	}
	defer resp.Body.Close()

	var respJson map[string]int64
	err = json.NewDecoder(resp.Body).Decode(&respJson)
	if err != nil {
		log.Println("Error while parsing context response.")
		panic(err)
	}
	return respJson, nil
}

func NewFunctionLookup(ns string, lister corelister.EndpointsLister, clientset *kubernetes.Clientset) *FunctionLookup {
	var MLServerIPTab map[string]string
    MLServerIPTab = make(map[string]string)
	functions := []string{"get-media-meta", "get-duration", "query-vacancy", "reserve-spot", "classify-image-ts", 
						"detect-object-ts", "anonymize-log", "filter-log", "detect-anomaly", "ingest-data"}
	for _, funcName := range functions {
		pods, err := clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("function=%s", funcName), })
        if err != nil {
			log.Println("Pods found error in NewFunctionLookup()")
            panic(err.Error())
        }
		// if len(pods.Items) != 1 {
		// 	log.Fatal("There is more than one client.")
        // }
        for _, pod := range pods.Items {
            pod, _ := clientset.CoreV1().Pods("default").Get(context.Background(), pod.Name, metav1.GetOptions{})
			log.Println(funcName, "ML IP", pod.Status.PodIP)
			MLServerIPTab[funcName] = pod.Status.PodIP
        }
	}

	return &FunctionLookup{
		DefaultNamespace: ns,
		EndpointLister:   lister,
		Listers:          map[string]corelister.EndpointsNamespaceLister{},
		lock:             sync.RWMutex{},
		ScaleLocker:      0,
		MLServerIPTab:    MLServerIPTab,
	}
}

type FunctionLookup struct {
	DefaultNamespace string
	EndpointLister   corelister.EndpointsLister
	Listers          map[string]corelister.EndpointsNamespaceLister

	lock sync.RWMutex
	ScaleLocker uint32
	MLServerIPTab map[string]string
}

func (f *FunctionLookup) GetMLServerIPTab(functionName string) map[string]string {
	return f.MLServerIPTab
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

func (l *FunctionLookup) Resolve(name string) (url.URL, map[string]float64, error) {
	functionName := name
	nullContext := make(map[string]float64)
	namespace := getNamespace(name, l.DefaultNamespace)
	if err := l.verifyNamespace(namespace); err != nil {
		return url.URL{}, nullContext, err
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
		return url.URL{}, nullContext, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
	}

	if len(svc.Subsets) == 0 {
		return url.URL{}, nullContext, fmt.Errorf("no subsets available for \"%s.%s\"", functionName, namespace)
	}

	// all := len(svc.Subsets[0].Addresses)
	if len(svc.Subsets[0].Addresses) == 0 {
		return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
	}

	// goldIPs := []string{}
	// for _, v := range svc.Subsets[0].Addresses {
	// 	goldIPs = append(goldIPs, v.IP)
	// }

	var serviceIP string = "none"

	ifSafePolicy := 0
	var maxLastTime int64 = -1

	// find the pods that has max lastTime and label == 0 and safe policy and available for new requests.
	for _, v := range svc.Subsets[0].Addresses {
		statusJson, err := QueryStatus(v.IP)
		log.Println(functionName, "statusJson", statusJson)
		if err != nil {
			log.Println("Current IP not available.")
			continue
		} else {
			if statusJson["Safe"] == 1 {
				ifSafePolicy = 1
				if statusJson["Label"] == 0 {  // find negative container
					log.Println(functionName, statusJson["LastTime"], maxLastTime, v.IP)
					if statusJson["LastTime"] > maxLastTime { // greedy select a container
						contextJson, err := QueryContext(v.IP)
						if err != nil {
							log.Println("Current IP not available.", err.Error())
							continue
							// log.Fatal(err.Error())
						}
						if (contextJson["MaxConn"] != contextJson["InFlight"]) {
							maxLastTime = statusJson["LastTime"]
							serviceIP = v.IP
						}
					}
				}
			}
		}
	}

	// the policy is not ok, choose noc containers
	if ifSafePolicy == 0 {
		if serviceIP != "none" {
			log.Fatal("ifSafePolicy == 0, but serviceIP is not none")
		}

		if !strings.HasSuffix(functionName, "-noc") {
			functionName = fmt.Sprintf("%s-noc", functionName) // ! Change function name here, because we change to noc options 
		}
		
		log.Println("Choose NOC function", functionName)
		svc, err := nsEndpointLister.Get(functionName)
		if err != nil {
			return url.URL{}, nullContext, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
		}

		// Scale from 0 if not IP available
		if len(svc.Subsets) == 0 || len(svc.Subsets[0].Addresses) == 0{
			if !atomic.CompareAndSwapUint32(&l.ScaleLocker, 0, 1) {
				return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\" now. Will scale later.", functionName, namespace)
			} 
			defer atomic.StoreUint32(&l.ScaleLocker, 0)
	
			svcLater, err := l.GetLister(namespace).Get(functionName)
			log.Println("[SCALE] Before", len(svc.Subsets[0].Addresses), "After", len(svcLater.Subsets[0].Addresses))
			if len(svcLater.Subsets[0].Addresses) > len(svc.Subsets[0].Addresses)  {
				return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\" now. Already scaled.", functionName, namespace)
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
	
			log.Println("[SCALE]", functionName, "scale from", len(svcLater.Subsets[0].Addresses), "to", uint64(len(svc.Subsets[0].Addresses) + 1))
			
			// return as original
			if len(svc.Subsets) == 0 {
				return url.URL{}, nullContext, fmt.Errorf("no subsets available for \"%s.%s\". Will Scale Now", functionName, namespace)
			}
			if len(svc.Subsets[0].Addresses) == 0 {
				return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\". Will Scale Now", functionName, namespace)
			}
		}

		var maxLastTime int64 = -1
		// find the pods that has max lastTime and available for new requests.
		for _, v := range svc.Subsets[0].Addresses {
			statusJson, err := QueryStatus(v.IP)
			if err != nil {
				log.Println("Current IP not available.")
				continue
			} else {
				if statusJson["LastTime"] > maxLastTime {
					contextJson, err := QueryContext(v.IP)
					if err != nil {
						log.Println("Current IP not available.", err.Error())
						continue
						// log.Fatal(err.Error())
					}
					if (contextJson["MaxConn"] != contextJson["InFlight"]) {
						maxLastTime = statusJson["LastTime"]
					serviceIP = v.IP
					}
				}
			}
		}
	}

	// target := rand.Intn(all)
	// serviceIP := svc.Subsets[0].Addresses[target].IP
	if serviceIP == "none" {
		if !atomic.CompareAndSwapUint32(&l.ScaleLocker, 0, 1) {
			return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\" now. Will scale later.", functionName, namespace)
		} 
		defer atomic.StoreUint32(&l.ScaleLocker, 0)

		svcLater, err := l.GetLister(namespace).Get(functionName)
		log.Println("[SCALE] Before", len(svc.Subsets[0].Addresses), "After", len(svcLater.Subsets[0].Addresses))
		if len(svcLater.Subsets[0].Addresses) > len(svc.Subsets[0].Addresses)  {
			return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\" now. Already scaled.", functionName, namespace)
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

		log.Println("[SCALE]", functionName, "scale from", len(svcLater.Subsets[0].Addresses), "to", uint64(len(svc.Subsets[0].Addresses) + 1))

		return url.URL{}, nullContext, fmt.Errorf("no addresses in subset for \"%s.%s\" now. Will scale now.", functionName, namespace)
	}

	urlStr := fmt.Sprintf("http://%s:%d", serviceIP, watchdogPort)

	urlRes, err := url.Parse(urlStr)
	if err != nil {
		return url.URL{}, nullContext, err
	}

	contextRes, err := QueryContext(serviceIP)
	contextRes["ifSafePolicy"] = float64(ifSafePolicy) // record to differentiate oc/noc requests
	// var contextBytes []byte
	if err != nil {
		return url.URL{}, nullContext, fmt.Errorf("Context not available")
	} 
	// else {
	// 	contextBytes, _ = json.Marshal( contextRes )
	// }
	// return *urlRes, string(contextBytes), nil
	return *urlRes, contextRes, nil
}

func (l *FunctionLookup) verifyNamespace(name string) error {
	if name != "kube-system" {
		return nil
	}
	// ToDo use global namepace parse and validation
	return fmt.Errorf("namespace not allowed")
}
