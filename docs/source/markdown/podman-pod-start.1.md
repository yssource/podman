% podman-pod-start(1)

## NAME
podman\-pod\-start - Start one or more pods

## SYNOPSIS
**podman pod start** [*options*] *pod* ...

## DESCRIPTION
Start containers in one or more pods.  You may use pod IDs or names as input. The pod must have a container attached
to be started.

## OPTIONS

#### **--all**, **-a**

Starts all pods

#### **--latest**, **-l**

Instead of providing the pod name or ID, start the last created pod. (This option is not available with the remote Podman client)

#### **--pod-id-file**

Read pod ID from the specified file and start the pod.  Can be specified multiple times.

## EXAMPLE

podman pod start mywebserverpod

podman pod start 860a4b23 5421ab4

podman pod start --latest

podman pod start --all

podman pod start --pod-id-file /path/to/id/file

## SEE ALSO
**[podman(1)](podman.1.md)**, **[podman-pod(1)](podman-pod.1.md)**, **[podman-pod-stop(1)](podman-pod-stop.1.md)**

## HISTORY
July 2018, Adapted from podman start man page by Peter Hunt <pehunt@redhat.com>
