package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/trento-project/trento/agent/discover"

	"github.com/hashicorp/consul-template/manager"
	consul "github.com/hashicorp/consul/api"
	"github.com/pkg/errors"

	consulInternal "github.com/trento-project/trento/internal/consul"
)

type Agent struct {
	cfg              Config
	check            Checker
	consulResultChan chan CheckResult
	wsResultChan     chan CheckResult
	webService       *webService
	consul           *consul.Client
	ctx              context.Context
	ctxCancel        context.CancelFunc
	templateRunner   *manager.Runner
}

type Config struct {
	WebHost             string
	WebPort             int
	ServiceName         string
	InstanceName        string
	DefinitionsPath     string
	TTL                 time.Duration
	TemplateSource      string
	TemplateDestination string
}

func New() (*Agent, error) {
	config, err := DefaultConfig()
	if err != nil {
		return nil, errors.Wrap(err, "could not create the agent configuration")
	}

	return NewWithConfig(config)
}

// returns a new instance of Agent with the given configuration
func NewWithConfig(cfg Config) (*Agent, error) {
	client, err := consul.NewClient(consul.DefaultConfig())
	if err != nil {
		return nil, errors.Wrap(err, "could not create a Consul client")
	}

	checker, err := NewChecker(cfg.DefinitionsPath)
	if err != nil {
		return nil, errors.Wrap(err, "could not create a Checker instance")
	}

	templateRunner, err := NewTemplateRunner(cfg.TemplateSource, cfg.TemplateDestination)
	if err != nil {
		return nil, errors.Wrap(err, "could not create the consul template runner")
	}

	ctx, ctxCancel := context.WithCancel(context.Background())

	wsResultChan := make(chan CheckResult, 1)

	agent := &Agent{
		cfg:            cfg,
		check:          checker,
		ctx:            ctx,
		ctxCancel:      ctxCancel,
		consul:         client,
		webService:     newWebService(wsResultChan),
		wsResultChan:   wsResultChan,
		templateRunner: templateRunner,
	}
	return agent, nil
}

func DefaultConfig() (Config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Config{}, errors.Wrap(err, "could not read the hostname")
	}

	return Config{
		InstanceName: hostname,
		TTL:          10 * time.Second,
	}, nil
}

func (a *Agent) Start() error {
	log.Println("Registering the agent service with Consul...")
	err := a.registerConsulService()
	if err != nil {
		return errors.Wrap(err, "could not register consul service")
	}
	log.Println("Consul service registered.")

	defer func() {
		log.Println("De-registering the agent service with Consul...")
		err := a.consul.Agent().ServiceDeregister(a.cfg.InstanceName)
		if err != nil {
			log.Println("An error occurred while trying to deregisterConsulService the agent service with Consul:", err)
		} else {
			log.Println("Consul service de-registered.")
		}
	}()

	var wg sync.WaitGroup
	// This number must match the number threads attached to the WaitGroup object
	wg.Add(4)
	errs := make(chan error, 4)

	go func(wg *sync.WaitGroup) {
		log.Println("Starting Check loop...")
		defer wg.Done()
		errs <- a.startCheckTicker()
		log.Println("Check loop stopped.")
	}(&wg)

	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		errs <- a.webService.Start(a.cfg.WebHost, a.cfg.WebPort, a.ctx)
	}(&wg)

	go func(wg *sync.WaitGroup) {
		log.Println("Starting consul-template loop...")
		defer wg.Done()
		errs <- a.startConsulTemplate()
		log.Println("consul-template loop stopped.")
	}(&wg)

	go func(wg *sync.WaitGroup) {
		log.Println("Starting SAP discovery loop...")
		defer wg.Done()
		errs <- a.startSapDiscovery()
		log.Println("SAP discovery loop stopped.")
	}(&wg)

	// As soon as all the goroutines in the Waitgroup are done, close the channel where the errors are sent
	go func() {
		wg.Wait()
		close(errs)
	}()

	// Scroll the errors channel and return as soon as one of the goroutines fails.
	// This will block until the errors channel is closed.
	for err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *Agent) Stop() {
	a.ctxCancel()
}

func (a *Agent) registerConsulService() error {
	var err error

	consulService := &consul.AgentServiceRegistration{
		ID:   a.cfg.InstanceName,
		Name: a.cfg.ServiceName,
		Tags: []string{"console-agent"},
		Checks: consul.AgentServiceChecks{
			&consul.AgentServiceCheck{
				CheckID: "ha_checks",
				Name:    "HA config checks",
				Notes:   "Checks whether or not the HA configuration is compliant with the provided best practices",
				TTL:     a.cfg.TTL.String(),
				Status:  consul.HealthWarning,
			},
		},
	}

	err = a.consul.Agent().ServiceRegister(consulService)
	if err != nil {
		return errors.Wrap(err, "could not register the agent service with Consul")
	}

	return nil
}
func (a *Agent) startCheckTicker() error {
	ticker := time.NewTicker(a.cfg.TTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			result, err := a.check()
			if err != nil {
				log.Println("An error occurred while running health checks:", err)
				continue
			}
			a.wsResultChan <- result
			a.updateConsulCheck(result)
		case <-a.ctx.Done():
			close(a.wsResultChan)
			return nil
		}
	}
}

func (a *Agent) updateConsulCheck(result CheckResult) {
	log.Println("Updating Consul check TTL...")

	var err error
	var status string

	summary := result.Summary()

	switch true {
	case summary.Fail > 0:
		status = consul.HealthCritical
	case summary.Warn > 0:
		status = consul.HealthWarning
	default:
		status = consul.HealthPassing
	}

	checkOutput := fmt.Sprintf("%s\nFor more detailed info, query this service at port %d.",
		result.String(), a.cfg.WebPort)
	err = a.consul.Agent().UpdateTTL("ha_checks", checkOutput, status)
	if err != nil {
		log.Println("An error occurred while trying to update TTL with Consul:", err)
		return
	}

	log.Printf("Consul check TTL updated. Status: %s.", status)
}

func (a *Agent) startSapDiscovery() error {
	ticker := time.NewTicker(time.Second*60)
	consulClient, _ := consulInternal.DefaultClient()
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sapSystems, err := discover.NewSAPSystemsDiscover()
			if err != nil {
				log.Println("An error occurred while running the SAP discovery loop:", err)
				continue
			}
			for _, s := range sapSystems {
				s.StoreDiscovery(consulClient)
			}
		case <-a.ctx.Done():
			return nil
		}
	}
}
