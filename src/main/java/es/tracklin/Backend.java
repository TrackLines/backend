package es.tracklin;

import es.tracklin.Configuration.MongoConfiguration;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.context.properties.EnableConfigurationProperties;

@SpringBootApplication
@EnableConfigurationProperties(MongoConfiguration.class)
public class Backend {
    public static void main(String[] args) {
        SpringApplication.run(Backend.class, args);
    }
}
