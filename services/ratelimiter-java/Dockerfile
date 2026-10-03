# Build context: this service directory.
FROM eclipse-temurin:25-jdk-alpine AS build
WORKDIR /src
COPY .mvn .mvn
COPY mvnw pom.xml ./
# Dependencies first, so they are cached until pom.xml changes.
RUN sh ./mvnw -B -q dependency:go-offline
COPY src src
RUN sh ./mvnw -B -q -DskipTests package && mv target/*.jar /app.jar

FROM eclipse-temurin:25-jre-alpine
RUN adduser -D -u 10001 app
COPY --from=build /app.jar /app/app.jar
USER 10001
ENV PORT=8080
EXPOSE 8080
# Size the heap from the container memory limit (important when benchmarking with --memory).
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75", "-jar", "/app/app.jar"]
