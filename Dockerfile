FROM golang:1.7
MAINTAINER Daigo Ikeda

RUN mkdir -p /go/src/hb
WORKDIR /go/src/hb

CMD ["go-wrapper", "run"]

RUN apt-get update
RUN yes | apt-get upgrade
RUN apt-get install unzip

RUN wget -P /tmp https://storage.googleapis.com/appengine-sdks/featured/go_appengine_sdk_linux_amd64-1.9.48.zip

RUN unzip /tmp/go_appengine_sdk_linux_amd64-1.9.48.zip -d /usr/local

ENV PATH $PATH:/usr/local/go_appengine:$GOPATH/bin

RUN go get -u github.com/constabulary/gb/...
RUN go get -u github.com/constabulary/gb/cmd/gb-vendor
RUN go get -u github.com/PalmStoneGames/gb-gae
RUN go get -u golang.org/x/tools/cmd/goimports
RUN go get -u github.com/golang/lint/golint
RUN go get -u github.com/favclip/jwg/cmd/jwg
RUN go get -u github.com/favclip/qbg/cmd/qbg
RUN go get -u github.com/favclip/smg/cmd/smg

COPY ./cmd/hb /go/src/hb

# githubログインに使うssh秘密鍵(rsa)ファイルをgithub_rsaのファイル名で
# カレントディレクトリに置いておく
RUN mkdir /root/.ssh
COPY github_rsa /root/.ssh/id_rsa
RUN chmod 0600 /root/.ssh/id_rsa

# github.comの公開鍵をgithub.pubkeyのファイル名でカレントディレクトリに置いておく
COPY github.pubkey /root/.ssh/known_hosts

# GCPプロジェクトのService Account(DatastoreとTaskqueueの権限を付与)の
# key jsonファイルをgcpkey.jsonのファイル名でカレントディレクトリに置いておく
COPY gcpkey.json /root/gcpkey.json
ENV GOOGLE_APPLICATION_CREDENTIALS /root/gcpkey.json

RUN go-wrapper download
RUN go-wrapper install

# Application to test

ARG gcp_project
ENV HB_GCP_PROJECT $gcp_project

ENV YOUR_REPO /root/your_app
ENV HB_APP $YOUR_REPO/server

ARG git_url
RUN git clone $git_url $YOUR_REPO

RUN cd $HB_APP && gb vendor restore
