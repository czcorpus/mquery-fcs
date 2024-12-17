FROM czcorpus/kontext-manatee:2.223.6-jammy

RUN apt-get update && apt-get install wget tar curl git bison libpcre3-dev -y \
    && wget https://go.dev/dl/go1.23.4.linux-amd64.tar.gz \
    && tar -C /usr/local -xzf go1.23.4.linux-amd64.tar.gz

WORKDIR /opt
RUN git clone https://github.com/czcorpus/manabuild \
    && cd manabuild \
    && export PATH=$PATH:/usr/local/go/bin \
    && make build && make install

WORKDIR /opt/mquery-sru
COPY . .
RUN git config --global --add safe.directory /opt/mquery-sru \
    && PATH=$PATH:/usr/local/go/bin:/root/go/bin \
    && ./configure \
    && make build