/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"net"
	"reflect"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sync"
)

const minikubeNodeName = "minikube"

var (
	machineIP string
	// map contains service NamespacedName -> Server
	servers = make(map[string]*Server)
)

type Server struct {
	log        logr.Logger
	listener   net.Listener
	minikubeIP string
	port       int32
	nodePort   int32
	done       chan struct{}
	wg         sync.WaitGroup
}

func (s *Server) serve() {
	defer s.wg.Done()
	s.log.Info(fmt.Sprintf("server listen on port %d", s.port))
	for {
		c, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return // chan is closed
			default:
				s.log.Error(err, "could not accept client connection")
				continue
			}
		}
		s.wg.Add(1)
		go s.handler(c)
	}
}

func (s *Server) handler(c net.Conn) {
	log := s.log.WithValues("connection", c)

	defer s.wg.Done()
	defer func() {
		err := c.Close()
		if err != nil {
			log.Error(err, "close connection")
		}
	}()

	target, err := net.Dial("tcp", fmt.Sprintf("%s:%d", s.minikubeIP, s.nodePort))
	if err != nil {
		log.Error(err, "could not connect to target")
		return
	}
	defer func() {
		err := target.Close()
		if err != nil {
			log.Error(err, "close target connection")
		}
	}()
	// forward target > connection
	go func() {
		_, err = io.Copy(target, c)
		if err != nil {
			log.Error(err, "copy from target to connection")
		}
	}()
	// forward connection > target
	_, err = io.Copy(c, target)
	if err != nil {
		log.Error(err, "copy from connection to target")
	}
}

func (s *Server) stop() {
	s.log.Info("stopping server")
	close(s.done)
	err := s.listener.Close()
	if err != nil {
		s.log.Error(err, "could not close listener")
	}
	s.wg.Wait()
}

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("service", req.NamespacedName)

	key := req.NamespacedName.String()
	service := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("Service resource not found. Ignoring since object must be deleted")
			if server, ok := servers[key]; ok {
				delete(servers, key)
				server.stop()
			}
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get Service")
		return ctrl.Result{}, err
	}

	if service.Spec.Type == corev1.ServiceTypeLoadBalancer {
		// init machine IP
		if len(machineIP) == 0 {
			ip, err := getMachineIP()
			if err != nil {
				log.Error(err, "get machine IP")
				return ctrl.Result{}, err
			}
			machineIP = ip
		}
		// check is server is already created
		if server, ok := servers[key]; ok {
			log.Info(fmt.Sprintf("server already created %+v", server))
		} else {
			// get minikube node IP
			var node corev1.Node
			if err := r.Get(ctx, types.NamespacedName{Name: minikubeNodeName}, &node); err != nil {
				log.Error(err, "Failed to get minikube Node")
				return ctrl.Result{}, err
			}
			minikubeIP := node.Status.Addresses[0].Address
			// get port & node port
			port := service.Spec.Ports[0].Port
			nodePort := service.Spec.Ports[0].NodePort
			log.Info(fmt.Sprintf("port: %d & node-port: %d", port, nodePort))
			// listener
			listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", machineIP, port))
			if err != nil {
				log.Error(err, "open port")
				return ctrl.Result{}, err
			}
			server = &Server{
				log:        log.WithName("server"),
				listener:   listener,
				minikubeIP: minikubeIP,
				port:       port,
				nodePort:   nodePort,
				done:       make(chan struct{}),
			}
			servers[req.NamespacedName.String()] = server
			server.wg.Add(1)
			go server.serve()
		}

		// update LoadBalancer service status
		loadBalancer := corev1.LoadBalancerStatus{
			Ingress: []corev1.LoadBalancerIngress{{
				IP: machineIP,
			}},
		}
		if !reflect.DeepEqual(loadBalancer, service.Status.LoadBalancer) {
			log.Info("updating LoadBalancer status")
			// The obj returned in r.Get is supposed to be a deep copy of the cache object, however this comment is suspicious
			// https://github.com/kubernetes-sigs/controller-runtime/blob/b2c90ab82af89fb84120108af14f4ade9df5d787/pkg/cache/internal/cache_reader.go#L84
			service.Status.LoadBalancer = loadBalancer
			err := r.Status().Update(ctx, service)
			if err != nil {
				log.Error(err, "Failed to update Service LoadBalancer status")
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}

func getMachineIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no IP found")
}
