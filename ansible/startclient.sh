#!/bin/bash
set -e

PROXYIP=`cat /home/ubuntu/inventory/proxy.txt`
PROXYPORT='8081'

bucket=`hostname`
pctput=0
duration=120m
minsize=8192
maxsize=8192

if [ -z "$1" ]; then
        echo "Using default pctput 0"
else
        pctput=$1
fi
if [ -z "$2" ]; then
        echo "Using default duration 120m"
else
        duration=$2
fi
if [ -z "$3" ]; then
        echo "Using default minzise -" $minsize
else
        minsize=$3
fi
if [ -z "$4" ]; then
        echo "Using default maxsize -" $maxsize
else
        maxsize=$4
fi

source /etc/profile.d/dfcpaths.sh
cd $DFCSRC/../cmd/dfcloader
sudo rm -rf screenlog.0
go run main.go worker.go -ip=$PROXYIP -port=$PROXYPORT -bucket=$bucket -local=true -minsize=$minsize -maxsize=$maxsize -statsinterval=1 -readertype=rand -cleanup=false -pctput=$pctput -duration=1m -totalputsize=4048000000 -numworkers=64
screen -mdSL client go run main.go worker.go -ip=$PROXYIP -port=$PROXYPORT -bucket=$bucket -local=true -minsize=$minsize -maxsize=$maxsize -statsinterval=1 -readertype=rand -cleanup=false -pctput=$pctput -duration=$duration -totalputsize=4048000000 -numworkers=64

echo "started dfcloader, wait for screnlog file to show up with timeout of 2min"
x=0
while [ "$x" -lt 24 -a ! -f screenlog.0 ]
do
  sleep 5
  x=$((x+1))

done

echo "screenlog file created"