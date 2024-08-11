package main

import (
	"context"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	v2 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	nsPrefix   string     = os.Getenv("NS_PREFIX")
	credsMutex sync.Mutex // Mutex to protect critical section during update
)

// Refreshes client credentials in a thread-safe manner
func refreshClientCredentials(clientset kubernetes.Interface) {
	credsMutex.Lock()         // Lock the mutex to ensure exclusive access to the critical section
	defer credsMutex.Unlock() // Unlock the mutex when the function exits

	newCreds := make(map[string]map[string]interface{}) // Temporary map to build new credentials

	// Implement retry logic for robustness
	var namespaces *v1.NamespaceList
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		namespaces, err = clientset.CoreV1().Namespaces().List(context.TODO(), v2.ListOptions{})
		if err == nil {
			break // Exit if successful
		}
		log.Printf("Attempt %d failed to list namespaces: %v", attempt+1, err)
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Printf("After retries, failed to list namespaces: %v", err)
		return
	}

	// Process each namespace and update credentials
	for _, ns := range namespaces.Items {
		if strings.HasPrefix(ns.Name, nsPrefix) {
			processSecret(clientset, ns.Name, os.Getenv("REDIS_SECRET"), newCreds)
			processTempSecrets(clientset, ns.Name, newCreds)
		}
	}

	// Atomically store the updated credentials map
	clientCreds.Store(newCreds)
	log.Printf("Credentials refreshed successfully")
}

func processSecret(clientset kubernetes.Interface, namespace, secretName string, newCreds map[string]map[string]interface{}) {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, v2.GetOptions{})
	if err != nil {
		log.Printf("Failed to fetch secret in namespace %s: %v", namespace, err)
		return
	}

	connStr, ok := secret.Data["redis-connection-string"]
	if !ok {
		log.Printf("Connection string not found in secret for namespace %s", namespace)
		return
	}

	parsedURL, err := url.Parse(string(connStr))
	if err != nil {
		log.Printf("Invalid Redis URL in namespace %s: %v", namespace, err)
		return
	}

	password, _ := parsedURL.User.Password()
	host := parsedURL.Hostname()
	dbIndex := strings.TrimPrefix(parsedURL.Path, "/")

	newCreds[password] = map[string]interface{}{
		"host":     host,
		"dbIndex":  dbIndex,
		"password": password,
	}
	log.Printf("Updated credentials for namespace %s", namespace)
}

func processTempSecrets(clientset kubernetes.Interface, namespace string, newCreds map[string]map[string]interface{}) {
	secrets, err := clientset.CoreV1().Secrets(namespace).List(context.TODO(), v2.ListOptions{})
	if err != nil {
		log.Printf("Failed to list secrets in namespace %s: %v", namespace, err)
		return
	}

	for _, secret := range secrets.Items {
		if strings.HasPrefix(secret.Name, "redis-secret-temp-") {
			processTempSecret(secret, newCreds)
		}
	}
}

func processTempSecret(secret v1.Secret, newCreds map[string]map[string]interface{}) {
	connStr, ok := secret.Data["redis-connection-string"]
	if !ok {
		log.Printf("Connection string not found in secret %s", secret.Name)
		return
	}

	parsedURL, err := url.Parse(string(connStr))
	if err != nil {
		log.Printf("Invalid Redis URL in secret %s: %v", secret.Name, err)
		return
	}

	password, _ := parsedURL.User.Password()
	host := parsedURL.Hostname()
	dbIndex := strings.TrimPrefix(parsedURL.Path, "/")

	validHoursStr, exists := secret.Data["valid_hours"]
	if !exists {
		newCreds[password] = map[string]interface{}{
			"host":     host,
			"dbIndex":  dbIndex,
			"password": password,
		}
	} else {
		validHours, err := strconv.Atoi(string(validHoursStr))
		if err != nil {
			log.Printf("Invalid valid_hours in secret %s: %v", secret.Name, err)
			return
		}
		newCreds[password] = map[string]interface{}{
			"host":        host,
			"dbIndex":     dbIndex,
			"password":    password,
			"valid_until": time.Now().Add(time.Duration(validHours) * time.Hour),
		}
	}
	log.Printf("Updated temporary credentials for secret %s", secret.Name)
}
