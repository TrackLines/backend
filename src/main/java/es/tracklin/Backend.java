package es.tracklin;

import es.tracklin.Configuration.MongoConfiguration;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.EnableAutoConfiguration;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.annotation.ComponentScan;

@SpringBootApplication
public class Backend {
    public static void main(String[] args) {
        SpringApplication.run(Backend.class, args);
    }
}
