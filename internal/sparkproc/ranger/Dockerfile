# syntax=docker/dockerfile:1
# The Dataproc RANGER optional component's Admin server and plugins (#2162),
# built on this machine from the pinned Apache Ranger 2.8.0 source release,
# as image version 3.0 runs it (2.2 and 2.3 run 2.4.0; the release notes,
# read 2026-11-23). Apache publishes no Ranger Admin image of its own
# release, so the image is built from source.
#
# Ranger's own reactor (its "all" profile) does not build as published: its
# Kylin plugin needs org.roaringbitmap:RoaringBitmap:0.7.36-os-kylin-r1,
# which is only in repo.spring.io, and that answers 401 to anonymous
# downloads; its distro module depends on every plugin, the Kylin plugin
# among them. Dataproc's Ranger component installs no Kylin plugin, so the
# build leaves it out without touching Ranger's build files: build.pom.xml
# is a reactor of Ranger's root and distro.pom.xml, which runs Ranger's own
# assembly descriptors for the Admin server, the HDFS, Hive and YARN
# plugins (each auditing to Solr) and Solr's audit config set, and
# "-pl cloudburrow-distro -am" builds only the modules those need.
# One source patch, hive4.patch: Ranger 2.8.0's Hive plugin is built for
# Hive 3.1.3, and HiveServer2 here is Hive 4.0.1 (the cluster's Hive), where
# HiveConf.ConfVars.PREEXECHOOKS is no more (NoSuchFieldError when
# HiveServer2 starts); the plugin reads and sets hive.exec.pre.hooks by
# its name instead, the same property in both releases.
# Enunciate (the REST API's generated documentation, not used by the
# server) is skipped: it compiles the published ranger-metrics sources
# jar against another Hadoop and fails; sortpom is skipped as it would
# reorder CloudBurrow's own two poms.
#
# Pins (dependencies.json's components.bigqueryml.dataprocRanger*):
# - the source release by SHA-256 (Apache's published SHA-512 is
#   c00acf62...e9cf4; the SHA-256 is of the same verified file);
# - the build and runtime base, the Docker official maven image
#   3.9.11-eclipse-temurin-8 (Ranger 2.8 builds with Java 8), by its
#   multi-arch index digest;
# - MySQL Connector/J 8.0.28 (Ranger's own pin) from Maven Central by
#   SHA-256: GPL-2.0 with the Universal FOSS Exception, downloaded into
#   this locally built image only, never into the repository or the CLI.
# - every file the build resolves into its Maven repository (the plugins,
#   the dependencies, and the Node release the frontend-maven-plugin
#   fetches), by SHA-256 in m2.sha256: after the build, a file that is not
#   there with its SHA-256 fails it. No Ranger artifact is in it, so every
#   Ranger module the image has is built here from the source release.
# Maven resolves them from Maven Central with checksum checking strict
# (-C), and the web UI's npm packages by its package-lock.json integrity
# hashes, as Ranger's build does; the Maven repository is a BuildKit cache
# mount, so a rebuild reuses it.
FROM maven@sha256:e3c149f44c95b0e9dd131862b3df67b3f061f7f6f3898a87b170564b3a943611 AS build
ADD --checksum=sha256:069ed1550160ac0da87980411c346d1e607466cb823720c73458a7b3b95b34aa \
    https://archive.apache.org/dist/ranger/2.8.0/apache-ranger-2.8.0.tar.gz /src/apache-ranger-2.8.0.tar.gz
RUN apt-get update && apt-get install -y --no-install-recommends python3 git && rm -rf /var/lib/apt/lists/* && \
    tar -xzf /src/apache-ranger-2.8.0.tar.gz -C /src && rm /src/apache-ranger-2.8.0.tar.gz
WORKDIR /src/apache-ranger-2.8.0
COPY build.pom.xml cloudburrow-build.xml
COPY distro.pom.xml cloudburrow-distro/pom.xml
COPY m2.sha256 /src/m2.sha256
COPY hive4.patch /src/hive4.patch
RUN git apply -p1 /src/hive4.patch
RUN --mount=type=cache,id=cloudburrow-ranger-2.8.0-m2,target=/root/.m2 \
    MAVEN_OPTS="-Xms1g -Xmx3g" mvn -B -C -q -f cloudburrow-build.xml -pl cloudburrow-distro -am \
      -DskipTests -DskipDocs -Dmaven.javadoc.skip=true -Drat.skip=true -Dcheckstyle.skip=true \
      -Dspotbugs.skip=true -Dpmd.skip=true -Djacoco.skip=true -Dsort.skip=true -Denunciate.skip=true package && \
    (cd /root/.m2/repository && \
      find . -type f \( -name '*.jar' -o -name '*.pom' -o -name '*.war' -o -name '*.zip' -o -name '*.tar.gz' -o -name '*.tgz' \) \
        -print0 | xargs -0 sha256sum | LC_ALL=C sort > /tmp/m2.have && \
      LC_ALL=C sort /src/m2.sha256 > /tmp/m2.want && LC_ALL=C comm -23 /tmp/m2.have /tmp/m2.want > /tmp/m2.unpinned && \
      if [ -s /tmp/m2.unpinned ]; then echo "not in m2.sha256:"; cat /tmp/m2.unpinned; exit 1; fi) && \
    mkdir /out && for p in admin hdfs-plugin hive-plugin yarn-plugin solr_audit_conf; do \
      cp target/ranger-2.8.0-$p.tar.gz /out/ || exit 1; done && rm -rf /src

FROM maven@sha256:e3c149f44c95b0e9dd131862b3df67b3f061f7f6f3898a87b170564b3a943611
RUN apt-get update && apt-get install -y --no-install-recommends python3 procps bc && rm -rf /var/lib/apt/lists/* && \
    mkdir -p /opt/ranger /usr/share/java /var/log/ranger /var/run/ranger
ADD --checksum=sha256:a00ccdf537ff50e50067b989108c2235197ffb65e197149bbb669db843cd1c3e \
    https://repo1.maven.org/maven2/mysql/mysql-connector-java/8.0.28/mysql-connector-java-8.0.28.jar /usr/share/java/mysql-connector.jar
RUN --mount=type=bind,from=build,source=/out,target=/out \
    for p in admin hdfs-plugin hive-plugin yarn-plugin; do \
      tar -xzf /out/ranger-2.8.0-$p.tar.gz -C /opt/ranger && ln -s /opt/ranger/ranger-2.8.0-$p /opt/ranger/$p || exit 1; done && \
    mkdir /opt/ranger/solr_audit_conf && tar -xzf /out/ranger-2.8.0-solr_audit_conf.tar.gz -C /opt/ranger/solr_audit_conf
COPY ranger.sh /opt/cloudburrow/ranger.sh
EXPOSE 6080
ENTRYPOINT ["bash", "/opt/cloudburrow/ranger.sh"]
