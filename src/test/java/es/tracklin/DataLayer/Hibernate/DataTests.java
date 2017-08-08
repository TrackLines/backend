package es.tracklin.DataLayer.Hibernate;

import es.tracklin.Backend;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.junit4.SpringRunner;

import static org.junit.Assert.assertNotEquals;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class DataTests {
    @Autowired
    es.tracklin.Configuration.Hibernate hibernateConfiguration;

    @Test
    public void shouldReturnPort() throws Exception {
        es.tracklin.DataConnections.Hibernate hibernate = new es.tracklin.DataConnections.Hibernate();
        hibernate.setHibernateConfiguration(hibernateConfiguration);
        assertNotEquals(0, hibernate.returnPort());
    }
}
