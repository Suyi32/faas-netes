// Copyright (c) Alex Ellis 2017. All rights reserved.
// Copyright 2020 OpenFaaS Author(s)
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package k8s

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	corelister "k8s.io/client-go/listers/core/v1"
)

// watchdogPort for the OpenFaaS function watchdog
const watchdogPort = 8080

func max(numbers map[string]int64) string {
	var maxNumber int64
	var maxIPAddr string

	if len(numbers) == 0 {
		fmt.Println("[ERROR] No available endpoint address.")
		os.Exit(1)
	}

	maxNumber = 0

	for ipAddr, n := range numbers {
		if n >= maxNumber {
			maxNumber = n
			maxIPAddr = ipAddr
		}
	}

	return maxIPAddr
}

func NewFunctionLookup(ns string, lister corelister.EndpointsLister) *FunctionLookup {
	var funcRouteMap2D map[string]map[string]int64
	funcRouteMap2D = make(map[string]map[string]int64)

	return &FunctionLookup{
		DefaultNamespace: ns,
		EndpointLister:   lister,
		Listers:          map[string]corelister.EndpointsNamespaceLister{},
		lock:             sync.RWMutex{},
		routeMap2D:       funcRouteMap2D,
	}
}

type FunctionLookup struct {
	DefaultNamespace string
	EndpointLister   corelister.EndpointsLister
	Listers          map[string]corelister.EndpointsNamespaceLister

	lock       sync.RWMutex
	routeMap2D map[string]map[string]int64
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

func (l *FunctionLookup) Resolve(name string) (url.URL, error) {
	functionName := name
	namespace := getNamespace(name, l.DefaultNamespace)
	if err := l.verifyNamespace(namespace); err != nil {
		return url.URL{}, err
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
		return url.URL{}, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
	}

	if len(svc.Subsets) == 0 {
		return url.URL{}, fmt.Errorf("no subsets available for \"%s.%s\"", functionName, namespace)
	}

	// all := len(svc.Subsets[0].Addresses)
	if len(svc.Subsets[0].Addresses) == 0 {
		return url.URL{}, fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
	}

	val, ok := l.routeMap2D[functionName]
	if ok {
		fmt.Println("Func", functionName, "'s ips ", val)
	} else {
		fmt.Println("make sub map")
		l.routeMap2D[functionName] = make(map[string]int64)
		for _, v := range svc.Subsets[0].Addresses {
			// fmt.Println("i", i, ", v:", v)
			_, ok := l.routeMap2D[functionName][v.IP]
			if !ok {
				l.routeMap2D[functionName][v.IP] = 0
			}
		}
	}

	// for _, v := range svc.Subsets[0].Addresses {
	// 	// fmt.Println("i", i, ", v:", v)
	// }

	// target := rand.Intn(all)

	// serviceIP := svc.Subsets[0].Addresses[target].IP

	serviceIP := max(l.routeMap2D[functionName])

	l.routeMap2D[functionName][serviceIP] = time.Now().UnixNano()

	urlStr := fmt.Sprintf("http://%s:%d", serviceIP, watchdogPort)

	urlRes, err := url.Parse(urlStr)
	if err != nil {
		return url.URL{}, err
	}

	return *urlRes, nil
}

func (l *FunctionLookup) verifyNamespace(name string) error {
	if name != "kube-system" {
		return nil
	}
	// ToDo use global namepace parse and validation
	return fmt.Errorf("namespace not allowed")
}
