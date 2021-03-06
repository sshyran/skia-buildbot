#!/bin/bash
#
# Script to set up a base image with just collectd, and pulld.
#
# This script is used on a temporary GCE instance. Just run it on a fresh
# Ubuntu 15.04 image and then capture a snapshot of the disk. Any image
# started with this snapshot as its image should be immediately setup to
# install applications via Skia Push.
#
# For more details see ../../push/DESIGN.md.
set -x -e
sudo apt-get update
sudo apt-get --assume-yes install git
# Running "sudo apt-get --assume-yes upgrade" may upgrade the package
# gce-startup-scripts, which would cause systemd to restart gce-startup-scripts,
# which would kill this script because it is a child process of
# gce-startup-scripts.
#
# IMPORTANT: We are using a public Ubuntu image which has automatic updates
# enabled by default. Thus we are not running any commands to update packages.

sudo apt-get --assume-yes -o Dpkg::Options::="--force-confold" install collectd
sudo gsutil cp gs://skia-push/debs/pulld/pulld:jcgregorio@jcgregorio.cnc.corp.google.com:2015-11-23T18:46:16Z:0483101f84c284640c4899ade97e4356655bfd00.deb pulld.deb
sudo dpkg -i pulld.deb
sudo systemctl start pulld.service

sudo apt-get install unattended-upgrades
sudo dpkg-reconfigure --priority=low unattended-upgrades

sudo apt-get --assume-yes install --fix-broken

# Setup collectd.
sudo cat <<EOF > collectd.conf
FQDNLookup false
Interval 60

LoadPlugin "logfile"
<Plugin "logfile">
  LogLevel "info"
  File "/var/log/collectd.log"
  Timestamp true
</Plugin>

LoadPlugin syslog

<Plugin syslog>
        LogLevel info
</Plugin>

LoadPlugin battery
LoadPlugin cpu
LoadPlugin df
LoadPlugin disk
LoadPlugin entropy
LoadPlugin interface
LoadPlugin irq
LoadPlugin load
LoadPlugin memory
LoadPlugin processes
LoadPlugin swap
LoadPlugin users
LoadPlugin write_graphite

<Plugin write_graphite>
        <Carbon>
                Host "skia-monitoring"
                Port "2003"
                Prefix "collectd."
                StoreRates false
                AlwaysAppendDS false
                EscapeCharacter "_"
                Protocol "tcp"
        </Carbon>
</Plugin>
EOF
sudo install -D --verbose --backup=none --group=root --owner=root --mode=600 collectd.conf /etc/collectd/collectd.conf
sudo /etc/init.d/collectd restart

# Install the Google Logging Agent and configure it to handle output logs.
curl -sSO https://dl.google.com/cloudagents/install-logging-agent.sh
sudo bash install-logging-agent.sh
sudo cat <<EOF > skia.conf
<source>
  type tail
  format none
  path /var/log/logserver/*.INFO
  pos_file /var/lib/google-fluentd/pos/skia.pos
  read_from_head true
  tag skia.*
</source>
EOF
sudo install -D --verbose --backup=none --group=default --owner=default --mode=600 skia.conf /etc/google-fluentd/config.d/skia.conf

# Install a placeholder for the auth credentials that the Google Logging Agent wants,
# On startup logserver will fill it in with the right values from project level metadata.
sudo cat <<EOF > application_default_credentials.json
{}
EOF
sudo install -D --verbose --backup=none --group=default --owner=default --mode=600 application_default_credentials.json /etc/google/auth/application_default_credentials.json
