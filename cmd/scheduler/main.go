package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezebunandu/k8s-alpha-scheduler/pkg/schedulercore"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const pollInterval = 2 * time.Second

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("failed to build kubeconfig: %v", err)
		}
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create kubernetes client: %v", err)
	}

	log.Printf("alphabetical-scheduler started")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	wait.Until(func() {
		results, err := schedulercore.RunOnce(ctx, client)
		if err != nil {
			log.Printf("scheduler error: %v", err)
		}

		for _, r := range results {
			switch r.Status {
			case schedulercore.StatusNoNode:
				log.Printf("no suitable node for pod %s/%s", r.Namespace, r.PodName)
			case schedulercore.StatusBindFailed:
				log.Printf("bind %s/%s -> %s: %v", r.Namespace, r.PodName, r.NodeName, r.Err)
			case schedulercore.StatusScheduled:
				log.Printf("scheduled %s/%s -> %s", r.Namespace, r.PodName, r.NodeName)
			}
		}
	}, pollInterval, ctx.Done())

	log.Printf("alphabetical-scheduler stopped")
}
