package es.tracklin;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.annotation.PropertySource;

@SpringBootApplication
@PropertySource("classpath:database.properties")
@PropertySource("classpath:elasticsearch.properties")
@PropertySource("classpath:tokens.properties")
@PropertySource(value = "classpath:local.properties", ignoreResourceNotFound = true)
public class Backend {
    public static void main(String[] args) {
        SpringApplication.run(Backend.class, args);
    }
}
