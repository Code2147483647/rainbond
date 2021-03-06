// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package f

import (
	"fmt"
	"time"

	"github.com/Sirupsen/logrus"
	v2beta1 "k8s.io/api/autoscaling/v2beta1"
	corev1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/goodrain/rainbond/gateway/annotations/parser"
	v1 "github.com/goodrain/rainbond/worker/appm/types/v1"
)

const (
	clientRetryCount    = 5
	clientRetryInterval = 5 * time.Second
)

// ApplyOne applies one rule.
func ApplyOne(clientset *kubernetes.Clientset, app *v1.AppService) error {
	_, err := clientset.CoreV1().Namespaces().Get(app.TenantID, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			_, err = clientset.CoreV1().Namespaces().Create(app.GetTenant())
			if err != nil && !k8sErrors.IsAlreadyExists(err) {
				return fmt.Errorf("error creating namespace: %v", err)
			}
		}
		if err != nil {
			return fmt.Errorf("error checking namespace: %v", err)
		}
	}
	if app.CustomParams != nil {
		if domain, exist := app.CustomParams["domain"]; exist {
			// update ingress
			for _, ing := range app.GetIngress(true) {
				if len(ing.Spec.Rules) > 0 && ing.Spec.Rules[0].Host == domain {
					if len(ing.Spec.TLS) > 0 {
						for _, secret := range app.GetSecrets(true) {
							if ing.Spec.TLS[0].SecretName == secret.Name {
								ensureSecret(secret, clientset)
							}
						}
					}
					ensureIngress(ing, clientset)
				}
			}
		}
		if domain, exist := app.CustomParams["tcp-address"]; exist {
			// update ingress
			for _, ing := range app.GetIngress(true) {
				if host, exist := ing.Annotations[parser.GetAnnotationWithPrefix("l4-host")]; exist {
					address := fmt.Sprintf("%s:%s", host, ing.Annotations[parser.GetAnnotationWithPrefix("l4-port")])
					if address == domain {
						ensureIngress(ing, clientset)
					}
				}
			}
		}
	} else {
		// update service
		for _, service := range app.GetServices(true) {
			ensureService(service, clientset)
		}
		// update secret
		for _, secret := range app.GetSecrets(true) {
			ensureSecret(secret, clientset)
		}
		// update endpoints
		for _, ep := range app.GetEndpoints(true) {
			if err := EnsureEndpoints(ep, clientset); err != nil {
				logrus.Errorf("create or update endpoint %s failure %s", ep.Name, err.Error())
			}
		}
		// update ingress
		for _, ing := range app.GetIngress(true) {
			ensureIngress(ing, clientset)
		}
	}
	// delete delIngress
	for _, ing := range app.GetDelIngs() {
		err := clientset.ExtensionsV1beta1().Ingresses(ing.Namespace).Delete(ing.Name, &metav1.DeleteOptions{})
		if err != nil && !k8sErrors.IsNotFound(err) {
			// don't return error, hope it is ok next time
			logrus.Warningf("error deleting ingress(%v): %v", ing, err)
		}
	}
	// delete delSecrets
	for _, secret := range app.GetDelSecrets() {
		err := clientset.CoreV1().Secrets(secret.Namespace).Delete(secret.Name, &metav1.DeleteOptions{})
		if err != nil && !k8sErrors.IsNotFound(err) {
			// don't return error, hope it is ok next time
			logrus.Warningf("error deleting secret(%v): %v", secret, err)
		}
	}
	// delete delServices
	for _, svc := range app.GetDelServices() {
		err := clientset.CoreV1().Services(svc.Namespace).Delete(svc.Name, &metav1.DeleteOptions{})
		if err != nil && !k8sErrors.IsNotFound(err) {
			// don't return error, hope it is ok next time
			logrus.Warningf("error deleting service(%v): %v", svc, err)
			continue
		}
		logrus.Debugf("successfully deleted service(%v)", svc)
	}
	return nil
}

func ensureService(new *corev1.Service, clientSet kubernetes.Interface) error {
	old, err := clientSet.CoreV1().Services(new.Namespace).Get(new.Name, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			_, err = clientSet.CoreV1().Services(new.Namespace).Create(new)
			if err != nil && !k8sErrors.IsAlreadyExists(err) {
				logrus.Warningf("error creating service %+v: %v", new, err)
			}
			return nil
		}
		logrus.Errorf("error getting service(%s): %v", fmt.Sprintf("%s/%s", new.Namespace, new.Name), err)
		return err
	}
	updateService := old.DeepCopy()
	updateService.Spec = new.Spec
	updateService.Labels = new.Labels
	updateService.Annotations = new.Annotations
	return persistUpdate(updateService, clientSet)
}

func persistUpdate(service *corev1.Service, clientSet kubernetes.Interface) error {
	var err error
	for i := 0; i < clientRetryCount; i++ {
		_, err = clientSet.CoreV1().Services(service.Namespace).UpdateStatus(service)
		if err == nil {
			return nil
		}
		// If the object no longer exists, we don't want to recreate it. Just bail
		// out so that we can process the delete, which we should soon be receiving
		// if we haven't already.
		if errors.IsNotFound(err) {
			logrus.Infof("Not persisting update to service '%s/%s' that no longer exists: %v",
				service.Namespace, service.Name, err)
			return nil
		}
		// TODO: Try to resolve the conflict if the change was unrelated to load
		// balancer status. For now, just pass it up the stack.
		if errors.IsConflict(err) {
			return fmt.Errorf("not persisting update to service '%s/%s' that has been changed since we received it: %v",
				service.Namespace, service.Name, err)
		}
		logrus.Warningf("Failed to update service '%s/%s' %s", service.Namespace, service.Name, err)
		time.Sleep(clientRetryInterval)
	}
	return err
}

func ensureIngress(ingress *extensions.Ingress, clientSet kubernetes.Interface) {
	_, err := clientSet.ExtensionsV1beta1().Ingresses(ingress.Namespace).Update(ingress)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			_, err := clientSet.ExtensionsV1beta1().Ingresses(ingress.Namespace).Create(ingress)
			if err != nil && !k8sErrors.IsAlreadyExists(err) {
				logrus.Errorf("error creating ingress %+v: %v", ingress, err)
			}
			return
		}
		logrus.Warningf("error updating ingress %+v: %v", ingress, err)
	}
}

func ensureSecret(secret *corev1.Secret, clientSet kubernetes.Interface) {
	_, err := clientSet.CoreV1().Secrets(secret.Namespace).Update(secret)

	if err != nil {
		if k8sErrors.IsNotFound(err) {
			_, err := clientSet.CoreV1().Secrets(secret.Namespace).Create(secret)
			if err != nil && !k8sErrors.IsAlreadyExists(err) {
				logrus.Warningf("error creating secret %+v: %v", secret, err)
			}
			return
		}
		logrus.Warningf("error updating secret %+v: %v", secret, err)
	}
}

// EnsureEndpoints creates or updates endpoints.
func EnsureEndpoints(ep *corev1.Endpoints, clientSet kubernetes.Interface) error {
	// See if there's actually an update here.
	currentEndpoints, err := clientSet.CoreV1().Endpoints(ep.Namespace).Get(ep.Name, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			currentEndpoints = &corev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:   ep.Name,
					Labels: ep.Labels,
				},
			}
		} else {
			return err
		}
	}

	createEndpoints := len(currentEndpoints.ResourceVersion) == 0

	if !createEndpoints &&
		apiequality.Semantic.DeepEqual(currentEndpoints.Subsets, ep.Subsets) &&
		apiequality.Semantic.DeepEqual(currentEndpoints.Labels, ep.Labels) {
		logrus.Debugf("endpoints are equal for %s/%s, skipping update", ep.Namespace, ep.Name)
		return nil
	}
	newEndpoints := currentEndpoints.DeepCopy()
	newEndpoints.Subsets = ep.Subsets
	newEndpoints.Labels = ep.Labels
	if newEndpoints.Annotations == nil {
		newEndpoints.Annotations = make(map[string]string)
	}
	if createEndpoints {
		// No previous endpoints, create them
		_, err = clientSet.CoreV1().Endpoints(ep.Namespace).Create(newEndpoints)
		logrus.Infof("Create endpoints for %v/%v", ep.Namespace, ep.Name)
	} else {
		// Pre-existing
		_, err = clientSet.CoreV1().Endpoints(ep.Namespace).Update(newEndpoints)
		logrus.Infof("Update endpoints for %v/%v", ep.Namespace, ep.Name)
	}
	if err != nil {
		if createEndpoints && errors.IsForbidden(err) {
			// A request is forbidden primarily for two reasons:
			// 1. namespace is terminating, endpoint creation is not allowed by default.
			// 2. policy is misconfigured, in which case no service would function anywhere.
			// Given the frequency of 1, we log at a lower level.
			logrus.Infof("Forbidden from creating endpoints: %v", err)
		}
		return err
	}
	return nil
}

// EnsureService ensure service:update or create service
func EnsureService(new *corev1.Service, clientSet kubernetes.Interface) error {
	return ensureService(new, clientSet)
}

// EnsureHPA -
func EnsureHPA(new *v2beta1.HorizontalPodAutoscaler, clientSet kubernetes.Interface) {
	_, err := clientSet.AutoscalingV2beta1().HorizontalPodAutoscalers(new.Namespace).Get(new.Name, metav1.GetOptions{})
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			_, err = clientSet.AutoscalingV2beta1().HorizontalPodAutoscalers(new.Namespace).Create(new)
			if err != nil {
				logrus.Warningf("error creating hpa %+v: %v", new, err)
			}
			return
		}
		logrus.Errorf("error getting hpa(%s): %v", fmt.Sprintf("%s/%s", new.Namespace, new.Name), err)
		return
	}
	_, err = clientSet.AutoscalingV2beta1().HorizontalPodAutoscalers(new.Namespace).Update(new)
	if err != nil {
		logrus.Warningf("error updating hpa %+v: %v", new, err)
		return
	}
}

// UpgradeIngress is used to update *extensions.Ingress.
func UpgradeIngress(clientset *kubernetes.Clientset,
	as *v1.AppService,
	old, new []*extensions.Ingress,
	handleErr func(msg string, err error) error) error {
	var oldMap = make(map[string]*extensions.Ingress, len(old))
	for i, item := range old {
		oldMap[item.Name] = old[i]
	}
	for _, n := range new {
		if o, ok := oldMap[n.Name]; ok {
			n.UID = o.UID
			n.ResourceVersion = o.ResourceVersion
			ing, err := clientset.ExtensionsV1beta1().Ingresses(n.Namespace).Update(n)
			if err != nil {
				if err := handleErr(fmt.Sprintf("error updating ingress: %+v: err: %v",
					ing, err), err); err != nil {
					return err
				}
				continue
			}
			as.SetIngress(ing)
			delete(oldMap, o.Name)
			logrus.Debugf("ServiceID: %s; successfully update ingress: %s", as.ServiceID, ing.Name)
		} else {
			logrus.Debugf("ingress: %+v", n)
			ing, err := clientset.ExtensionsV1beta1().Ingresses(n.Namespace).Create(n)
			if err != nil {
				if err := handleErr(fmt.Sprintf("error creating ingress: %+v: err: %v",
					ing, err), err); err != nil {
					return err
				}
				continue
			}
			as.SetIngress(ing)
			logrus.Debugf("ServiceID: %s; successfully create ingress: %s", as.ServiceID, ing.Name)
		}
	}
	for _, ing := range oldMap {
		if ing != nil {
			if err := clientset.ExtensionsV1beta1().Ingresses(ing.Namespace).Delete(ing.Name,
				&metav1.DeleteOptions{}); err != nil {
				if err := handleErr(fmt.Sprintf("error deleting ingress: %+v: err: %v",
					ing, err), err); err != nil {
					return err
				}
				continue
			}
			logrus.Debugf("ServiceID: %s; successfully delete ingress: %s", as.ServiceID, ing.Name)
		}
	}
	return nil
}

// UpgradeSecrets is used to update *corev1.Secret.
func UpgradeSecrets(clientset *kubernetes.Clientset,
	as *v1.AppService, old, new []*corev1.Secret,
	handleErr func(msg string, err error) error) error {
	var oldMap = make(map[string]*corev1.Secret, len(old))
	for i, item := range old {
		oldMap[item.Name] = old[i]
	}
	for _, n := range new {
		if o, ok := oldMap[n.Name]; ok {
			n.UID = o.UID
			n.ResourceVersion = o.ResourceVersion
			sec, err := clientset.CoreV1().Secrets(n.Namespace).Update(n)
			if err != nil {
				if err := handleErr(fmt.Sprintf("error updating secret: %+v: err: %v",
					sec, err), err); err != nil {
					return err
				}
				continue
			}
			as.SetSecret(sec)
			delete(oldMap, o.Name)
			logrus.Debugf("ServiceID: %s; successfully update secret: %s", as.ServiceID, sec.Name)
		} else {
			sec, err := clientset.CoreV1().Secrets(n.Namespace).Create(n)
			if err != nil {
				if err := handleErr(fmt.Sprintf("error creating secret: %+v: err: %v",
					sec, err), err); err != nil {
					return err
				}
				continue
			}
			as.SetSecret(sec)
			logrus.Debugf("ServiceID: %s; successfully create secret: %s", as.ServiceID, sec.Name)
		}
	}
	for _, sec := range oldMap {
		if sec != nil {
			if err := clientset.CoreV1().Secrets(sec.Namespace).Delete(sec.Name, &metav1.DeleteOptions{}); err != nil {
				if err := handleErr(fmt.Sprintf("error deleting secret: %+v: err: %v",
					sec, err), err); err != nil {
					return err
				}
				continue
			}
			logrus.Debugf("ServiceID: %s; successfully delete secret: %s", as.ServiceID, sec.Name)
		}
	}
	return nil
}
