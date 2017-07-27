package es.tracklin.Settings;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.vault.core.VaultOperations;
import org.springframework.vault.support.VaultResponseSupport;

import java.util.HashMap;
import java.util.Map;

public class Settings {
    @Autowired
    private VaultOperations operations;

    public void witeSecrets(String userId, String password) {
        Map<String, String> data = new HashMap<String, String>();
        data.put("password", password);
        operations.write(userId, data);
    }

    public Person readSecrets(String userId) {
        VaultResponseSupport<Person> response = operations.read(userId, Person.class);
        return response.getBody();
    }
}
