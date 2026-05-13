# Substrate Benchmarking

This is the nascent suite for benchmarking Substrate's performance at scale.
It has two components:

* A metrics scraping stack that leverages Prometheus and Grafana to collect and
  visualize metrics (currently only from the locust workers)
* A locust test suite that generates load against Substrate

## Rebuilding gRPC Python clients

Make sure you have a virtual environment created (`python3 -m venv venv`)
and activated (`source venv/bin/activate`).

Install project requirements: `pip install -r requirements.txt`

Then run `generate_protos.sh` to generate the Python proto clients.

## Install monitoring

This must be done first to create the `monitoring` namespace

```bash
kubectl apply -f monitoring.yaml
```

## Deploy Workloads and Demos

Before running any benchmarks, the corresponding workloads or actor templates must be deployed to the cluster. If you run the benchmarks without deploying their dependent templates first, the Locust tasks will fail to create or locate the required actors.

> [!IMPORTANT]
> **Caveat:** Workloads/demos must be deployed before the associated benchmarks can be run. Make sure you have sourced the environment configuration file (e.g., `source .ate-dev-env.sh`) in your active terminal to ensure all environment variables (like `BUCKET_NAME` and `PROJECT_ID`) are properly defined.

### 1. Deploying Scale Workloads (For `ate-api` or `all` benchmarks)
The scale benchmarking tasks (such as `kernelmem`, `sleep`, and `usermem`) require deploying the scale workload templates.

* **Deploy Workloads:**
  ```bash
  ./benchmarking/workloads/deploy.sh --deploy
  ```
* **Remove Workloads:**
  ```bash
  ./benchmarking/workloads/deploy.sh --delete
  ```

### 2. Deploying the Counter Demo (For `counter` benchmark)
The counter load testing scenario uses the stateful `counter` actor template.

* **Deploy Counter Demo:**
  ```bash
  ./hack/install-ate.sh --deploy-demo-counter
  ```
* **Remove Counter Demo:**
  ```bash
  ./hack/install-ate.sh --delete-demo-counter
  ```

## Build/Deploy locust

First, make sure you have sourced `.ate-dev-env.sh` to set up the necessary environment variables:

* Run `build_and_push.sh` to build and push the image
* Run `deploy_locust.sh` to deploy the locust worker
  * You can control what load test is deployed by setting the `LOAD_TYPE` environment variable:
    * `LOAD_TYPE=all` (Default) to deploy all tests with the Locust web UI enabled.
    * `LOAD_TYPE=ate-api` to deploy the standalone ate-api load test (headless).
    * `LOAD_TYPE=counter` to deploy the standalone counter demo load test (headless).


## Viewing the results

### Grafana Dashboards
* Run `kubectl port-forward svc/grafana -n monitoring 3000:3000`
* Visit `http://localhost:3000` in your browser.

### Locust Web UI (for `LOAD_TYPE=all`)
* Run `kubectl port-forward svc/locust-all -n monitoring 8089:8089`
* Visit `http://localhost:8089` in your browser to configure and start the load test.

## Future work

Running discrete load tests and storing the results in a database.

This will require setting up a Service Account to push results to a
GCS bucket and a controller script to strictly orchestrate the locust
invocation(s), make observations against Prometheus, then push the results.
