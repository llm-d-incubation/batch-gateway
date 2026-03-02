variable "REGISTRY" {
  default = "ghcr.io/llm-d-incubation"
}

variable "TAG" {
  default = "latest"
}

variable "COMMIT_SHA" {
  default = ""
}

variable "BUILD_DATE" {
  default = ""
}

variable "VERSION" {
  default = "dev"
}

# Docker metadata for labels
variable "SOURCE_REPO" {
  default = "https://github.com/llm-d-incubation/batch-gateway"
}

# Common group to build all images
group "default" {
  targets = ["apiserver", "processor", "gc"]
}

# API Server target
target "apiserver" {
  context    = "."
  dockerfile = "docker/Dockerfile.apiserver"

  platforms = ["linux/amd64", "linux/arm64"]

  tags = [
    # Always tag with commit SHA if provided
    notequal("", COMMIT_SHA) ? "${REGISTRY}/batch-gateway-apiserver:${COMMIT_SHA}" : "",
    # Tag with version (could be 'latest', a release tag like 'v1.0.0', or 'dev')
    "${REGISTRY}/batch-gateway-apiserver:${TAG}",
  ]

  labels = {
    "org.opencontainers.image.created"       = "${BUILD_DATE}"
    "org.opencontainers.image.source"        = "${SOURCE_REPO}"
    "org.opencontainers.image.version"       = "${VERSION}"
    "org.opencontainers.image.revision"      = "${COMMIT_SHA}"
    "org.opencontainers.image.title"         = "Batch Gateway API Server"
    "org.opencontainers.image.description"   = "LLM-D implementation of OpenAI /v1/batch and /v1/file APIs"
    "org.opencontainers.image.vendor"        = "llm-d"
  }

  # Enable layer caching
  cache-from = [
    "type=registry,ref=${REGISTRY}/batch-gateway-apiserver:buildcache"
  ]
  cache-to = [
    "type=registry,ref=${REGISTRY}/batch-gateway-apiserver:buildcache,mode=max"
  ]
}

# Batch Processor target
target "processor" {
  context    = "."
  dockerfile = "docker/Dockerfile.processor"

  platforms = ["linux/amd64", "linux/arm64"]

  tags = [
    # Always tag with commit SHA if provided
    notequal("", COMMIT_SHA) ? "${REGISTRY}/batch-gateway-processor:${COMMIT_SHA}" : "",
    # Tag with version (could be 'latest', a release tag like 'v1.0.0', or 'dev')
    "${REGISTRY}/batch-gateway-processor:${TAG}",
  ]

  labels = {
    "org.opencontainers.image.created"       = "${BUILD_DATE}"
    "org.opencontainers.image.source"        = "${SOURCE_REPO}"
    "org.opencontainers.image.version"       = "${VERSION}"
    "org.opencontainers.image.revision"      = "${COMMIT_SHA}"
    "org.opencontainers.image.title"         = "Batch Gateway Processor"
    "org.opencontainers.image.description"   = "Background processor for LLM batch job execution"
    "org.opencontainers.image.vendor"        = "llm-d"
  }

  # Enable layer caching
  cache-from = [
    "type=registry,ref=${REGISTRY}/batch-gateway-processor:buildcache"
  ]
  cache-to = [
    "type=registry,ref=${REGISTRY}/batch-gateway-processor:buildcache,mode=max"
  ]
}

# Garbage Collector target
target "gc" {
  context    = "."
  dockerfile = "docker/Dockerfile.gc"

  platforms = ["linux/amd64", "linux/arm64"]

  tags = [
    # Always tag with commit SHA if provided
    notequal("", COMMIT_SHA) ? "${REGISTRY}/batch-gateway-gc:${COMMIT_SHA}" : "",
    # Tag with version (could be 'latest', a release tag like 'v1.0.0', or 'dev')
    "${REGISTRY}/batch-gateway-gc:${TAG}",
  ]

  labels = {
    "org.opencontainers.image.created"       = "${BUILD_DATE}"
    "org.opencontainers.image.source"        = "${SOURCE_REPO}"
    "org.opencontainers.image.version"       = "${VERSION}"
    "org.opencontainers.image.revision"      = "${COMMIT_SHA}"
    "org.opencontainers.image.title"         = "Batch Gateway Garbage Collector"
    "org.opencontainers.image.description"   = "Garbage collector for expired batch jobs and files"
    "org.opencontainers.image.vendor"        = "llm-d"
  }

  # Enable layer caching
  cache-from = [
    "type=registry,ref=${REGISTRY}/batch-gateway-gc:buildcache"
  ]
  cache-to = [
    "type=registry,ref=${REGISTRY}/batch-gateway-gc:buildcache,mode=max"
  ]
}
