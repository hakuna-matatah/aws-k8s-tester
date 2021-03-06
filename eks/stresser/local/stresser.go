// Package local implements cluster local load tests.
// ref. https://github.com/kubernetes/perf-tests
package local

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-k8s-tester/eks/stresser"
	eks_tester "github.com/aws/aws-k8s-tester/eks/tester"
	"github.com/aws/aws-k8s-tester/eksconfig"
	k8s_client "github.com/aws/aws-k8s-tester/pkg/k8s-client"
	"github.com/aws/aws-k8s-tester/pkg/timeutil"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Config defines stresser configuration.
// ref. https://github.com/kubernetes/perf-tests
type Config struct {
	Logger *zap.Logger
	Stopc  chan struct{}

	EKSConfig *eksconfig.Config
	K8SClient k8s_client.EKS
}

// TODO: use kubemark
// nodelease.NewController, kubemark.GetHollowKubeletConfig

func New(cfg Config) eks_tester.Tester {
	cfg.Logger.Info("creating tester", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
	return &tester{cfg: cfg}
}

type tester struct {
	cfg Config
}

func (ts *tester) Create() (err error) {
	if !ts.cfg.EKSConfig.IsEnabledAddOnStresserLocal() {
		ts.cfg.Logger.Info("skipping tester.Create", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
		return nil
	}
	if ts.cfg.EKSConfig.AddOnStresserLocal.Created {
		ts.cfg.Logger.Info("skipping tester.Create", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
		return nil
	}

	ts.cfg.Logger.Info("starting tester.Create", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
	ts.cfg.EKSConfig.AddOnStresserLocal.Created = true
	ts.cfg.EKSConfig.Sync()
	createStart := time.Now()
	defer func() {
		createEnd := time.Now()
		ts.cfg.EKSConfig.AddOnStresserLocal.TimeFrameCreate = timeutil.NewTimeFrame(createStart, createEnd)
		ts.cfg.EKSConfig.Sync()
	}()

	if err := k8s_client.CreateNamespace(
		ts.cfg.Logger,
		ts.cfg.K8SClient.KubernetesClientSet(),
		ts.cfg.EKSConfig.AddOnStresserLocal.Namespace,
	); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	nss, err := ts.cfg.K8SClient.KubernetesClientSet().CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	cancel()
	if err != nil {
		ts.cfg.Logger.Warn("list namespaces failed", zap.Error(err))
		return err
	}
	ns := make([]string, 0, len(nss.Items))
	for _, nv := range nss.Items {
		ns = append(ns, nv.GetName())
	}

	loader := stresser.New(stresser.Config{
		Logger:         ts.cfg.Logger,
		Stopc:          ts.cfg.Stopc,
		Client:         ts.cfg.K8SClient,
		ClientTimeout:  ts.cfg.EKSConfig.ClientTimeout,
		Deadline:       time.Now().Add(ts.cfg.EKSConfig.AddOnStresserLocal.Duration),
		NamespaceWrite: ts.cfg.EKSConfig.AddOnStresserLocal.Namespace,
		NamespacesRead: ns,
		ObjectSize:     ts.cfg.EKSConfig.AddOnStresserLocal.ObjectSize,
		ListLimit:      ts.cfg.EKSConfig.AddOnStresserLocal.ListLimit,
		WritesJSONPath: ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesJSONPath,
		ReadsJSONPath:  ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsJSONPath,
	})
	loader.Start()

	select {
	case <-ts.cfg.Stopc:
		ts.cfg.Logger.Warn("cluster stresser aborted")
		loader.Stop()
		ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary, ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary, err = loader.CollectMetrics()
		ts.cfg.EKSConfig.Sync()
		if err != nil {
			ts.cfg.Logger.Warn("failed to get metrics", zap.Error(err))
		} else {
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummaryJSONPath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary.JSON()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummaryTablePath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary.Table()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			fmt.Printf("\n\nAddOnStresserLocal.RequestsWritesSummary:\n%s\n", ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary.Table())
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummaryJSONPath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary.JSON()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummaryTablePath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary.Table()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			fmt.Printf("\n\nAddOnStresserLocal.RequestsReadsSummary:\n%s\n", ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary.Table())
		}
		return nil

	case <-time.After(ts.cfg.EKSConfig.AddOnStresserLocal.Duration):
		ts.cfg.Logger.Info("completing load testing", zap.Duration("duration", ts.cfg.EKSConfig.AddOnStresserLocal.Duration))
		loader.Stop()
		ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary, ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary, err = loader.CollectMetrics()
		ts.cfg.EKSConfig.Sync()
		if err != nil {
			ts.cfg.Logger.Warn("failed to get metrics", zap.Error(err))
		} else {
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummaryJSONPath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary.JSON()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummaryTablePath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary.Table()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			fmt.Printf("\n\nAddOnStresserLocal.RequestsWritesSummary:\n%s\n", ts.cfg.EKSConfig.AddOnStresserLocal.RequestsWritesSummary.Table())
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummaryJSONPath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary.JSON()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			err = ioutil.WriteFile(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummaryTablePath, []byte(ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary.Table()), 0600)
			if err != nil {
				ts.cfg.Logger.Warn("failed to write file", zap.Error(err))
				return err
			}
			fmt.Printf("\n\nAddOnStresserLocal.RequestsReadsSummary:\n%s\n", ts.cfg.EKSConfig.AddOnStresserLocal.RequestsReadsSummary.Table())
		}

		select {
		case <-ts.cfg.Stopc:
			ts.cfg.Logger.Warn("cluster stresser aborted")
			return nil
		case <-time.After(30 * time.Second):
		}
	}

	waitDur, retryStart := 5*time.Minute, time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			ts.cfg.Logger.Warn("health check aborted")
			return nil
		case <-time.After(5 * time.Second):
		}
		err = ts.cfg.K8SClient.CheckHealth()
		if err == nil {
			break
		}
		ts.cfg.Logger.Warn("health check failed", zap.Error(err))
	}
	ts.cfg.EKSConfig.Sync()
	if err == nil {
		ts.cfg.Logger.Info("health check success after load testing")
	} else {
		ts.cfg.Logger.Warn("health check failed after load testing", zap.Error(err))
	}
	return err
}

func (ts *tester) Delete() error {
	if !ts.cfg.EKSConfig.IsEnabledAddOnStresserLocal() {
		ts.cfg.Logger.Info("skipping tester.Delete", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
		return nil
	}
	if !ts.cfg.EKSConfig.AddOnStresserLocal.Created {
		ts.cfg.Logger.Info("skipping tester.Delete", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
		return nil
	}

	ts.cfg.Logger.Info("starting tester.Delete", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
	deleteStart := time.Now()
	defer func() {
		deleteEnd := time.Now()
		ts.cfg.EKSConfig.AddOnStresserLocal.TimeFrameDelete = timeutil.NewTimeFrame(deleteStart, deleteEnd)
		ts.cfg.EKSConfig.Sync()
	}()

	var errs []string

	if err := k8s_client.DeleteNamespaceAndWait(
		ts.cfg.Logger,
		ts.cfg.K8SClient.KubernetesClientSet(),
		ts.cfg.EKSConfig.AddOnStresserLocal.Namespace,
		k8s_client.DefaultNamespaceDeletionInterval,
		k8s_client.DefaultNamespaceDeletionTimeout,
	); err != nil {
		return fmt.Errorf("failed to delete stresser namespace (%v)", err)
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}

	ts.cfg.EKSConfig.AddOnStresserLocal.Created = false
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) AggregateResults() (err error) {
	if !ts.cfg.EKSConfig.IsEnabledAddOnStresserLocal() {
		ts.cfg.Logger.Info("skipping tester.AggregateResults", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
		return nil
	}
	if !ts.cfg.EKSConfig.AddOnStresserLocal.Created {
		ts.cfg.Logger.Info("skipping tester.AggregateResults", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
		return nil
	}

	ts.cfg.Logger.Info("starting tester.AggregateResults", zap.String("tester", reflect.TypeOf(tester{}).PkgPath()))
	return nil
}
