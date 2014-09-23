#!/bin/bash -e -t
export SSH_KEYFILE="${SSH_KEYFILE:-$HOME/tmp/isucon-catatsuy.pem}"
export SSH_SERVER=isucon@54.64.143.220
export RSYNC_RSH="ssh -i $SSH_KEYFILE"


curl --data "deploy by $USER" $'https://teamfreesozai.slack.com/services/hooks/slackbot?token=sB7dwgicFYH7OErdGXPbp0mo&channel=%23general'
rsync -avz ./ $SSH_SERVER:/home/isucon/webapp/go
ssh -t -i $SSH_KEYFILE $SSH_SERVER /home/isucon/build.sh
