#!/bin/bash

set -o errexit -o nounset
TRAVIS_BRANCH=master
if [ "$TRAVIS_BRANCH" != "master" ]
then 
	    echo "This commit was made against the $TRAVIS_BRANCH and not the master! No deploy!" 
		exit 1
fi

rev=$(git rev-parse --short HEAD)
sed -i "s/\DOCKER_OPTS=\"/DOCKER_OPTS=\"--insecure-registry=hub.sky-cloud.net /g" /etc/default/docker
cat /etc/default/docker
service docker restart
echo "$HUB" | docker login -u docker-image-builder  http://hub.sky-cloud.net --password-stdin
docker build -t hub.sky-cloud.net/nap2/netd:${TRAVIS_BRANCH}_build-${rev} .
docker push hub.sky-cloud.net/nap2/netd:${TRAVIS_BRANCH}_build-${rev}


