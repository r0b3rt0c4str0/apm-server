user_name = "USER"

# The number of AZs the APM Server should span.
apm_server_zone_count = 1
# The Elasticsearch cluster node size.
elasticsearch_size = "128g"
# The number of AZs the Elasticsearch cluster should have.
elasticsearch_zone_count = 2
# APM server instance size
apm_server_size = "16g"
# Benchmarks executor size executor
worker_instance_type = "c6i.2xlarge"
apm_shards = 2

ess_region = "gcp-us-west1"
deployment_template = "gcp-cpu-optimized-v5"
